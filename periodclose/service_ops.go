package periodclose

import (
	"strconv"
	"time"
)

// nextAuditSeq returns the next monotonic audit sequence. Caller holds s.mu.
func (s *Service) nextAuditSeq() int64 {
	s.auditSeq++
	return s.auditSeq
}

// auditFrame builds one audit frame WITHOUT touching in-memory state; the
// record enters the trail only when the batch is applied after a durable
// append (via applyFrame). Audit shares the same fsynced batch as the state
// change it describes, so the two can never diverge. Caller holds s.mu.
func (s *Service) auditFrame(actor Actor, op OpKind, p Period, ruleVer, closeVer int64, detail string, allowed bool) (journalFrame, error) {
	rec := AuditRecord{
		Seq:          s.nextAuditSeq(),
		At:           s.clock(),
		ActorID:      actor.ID,
		Op:           op,
		Period:       p,
		RuleVersion:  ruleVer,
		CloseVersion: closeVer,
		Detail:       detail,
		Allowed:      allowed,
	}
	return marshalFrame(frameAudit, auditPayload{Record: rec})
}

// recordDeny durably logs a rejected attempt without touching business
// state. It opens its own committed batch.
func (s *Service) recordDeny(actor Actor, op OpKind, p Period, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return
	}
	af, err := s.auditFrame(actor, OpDeny, p, 0, 0, string(op)+": "+detail, false)
	if err != nil {
		return
	}
	end, err := s.journal.appendBatch([]journalFrame{af})
	if err != nil {
		return
	}
	if err := s.applyFrame(af); err != nil {
		return
	}
	s.lastLSN = end
}

// checkpointLocked writes a fresh snapshot after a committed mutation. A
// snapshot failure never hides a successful commit (the journal is the
// source of truth); replay alone rebuilds identical state.
func (s *Service) checkpointLocked() {
	snap := &snapshot{
		LastLSN:    s.lastLSN,
		SeenEntry:  s.seenEnt,
		SeenAdjust: s.seenAdj,
		Rules:      s.rules.snapshot(),
		Periods:    s.periods,
		AuditSeq:   s.auditSeq,
	}
	_ = saveSnapshot(s.dir, snap)
}

// commitLocked appends one prepared batch, folds it into memory exactly
// once, advances lastLSN and checkpoints. Caller holds s.mu.
func (s *Service) commitLocked(frames []journalFrame) error {
	end, err := s.journal.appendPrepared(frames)
	if err != nil {
		return err
	}
	for _, fr := range frames {
		if err := s.applyFrame(fr); err != nil {
			return err
		}
	}
	s.lastLSN = end
	s.checkpointLocked()
	return nil
}

// SubmitEntries ingests detail lines idempotently by entry ID. Resubmitted
// duplicates are acknowledged but never counted twice. Lines attributed to
// an already-closed period are rejected (logged) so late, duplicate or
// out-of-order arrivals cannot move a closed result. All accepted lines
// commit in a single atomic batch.
func (s *Service) SubmitEntries(actor Actor, entries ...Entry) error {
	if err := actor.require(RoleSubmitter, RoleAdmin); err != nil {
		s.recordDeny(actor, OpIntake, "", "unauthorized")
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	for _, e := range entries {
		if e.ID == "" || e.Period == "" {
			return ErrBadArgument
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	lsn := s.journal.peekNext()
	frames := make([]journalFrame, 0, len(entries)*2)
	pending := map[string]bool{} // ids already accepted within this batch
	var accepted int
	for _, e := range entries {
		if _, dup := s.seenEnt[e.ID]; dup || pending[e.ID] {
			continue // idempotent duplicate: no frame, no second count
		}
		if pp := s.periods[e.Period]; pp != nil &&
			(pp.Status == StatusClosed || pp.Status == StatusClosing) {
			af, _ := s.auditFrame(actor, OpDeny, e.Period, 0, pp.CloseSeq,
				"entry rejected for non-open period ("+string(pp.Status)+"): "+e.ID, false)
			af.LSN = lsn
			lsn++
			frames = append(frames, af)
			continue
		}
		stored := StoredEntry{Entry: e, Seq: lsn, ReceivedAt: now}
		ef, err := marshalFrame(frameEntry, entryPayload{Entry: stored})
		if err != nil {
			return err
		}
		ef.LSN = lsn
		lsn++
		frames = append(frames, ef)
		pending[e.ID] = true
		accepted++
	}
	if len(frames) == 0 {
		return nil // all duplicates
	}
	if err := s.commitLocked(frames); err != nil {
		return err
	}
	_ = accepted
	return nil
}

// AddRule publishes a new versioned rule. Closed periods keep their pinned
// RuleVersion; open periods pick the latest-effective rule at close time.
func (s *Service) AddRule(actor Actor, rule Rule) (Rule, error) {
	if err := actor.require(RoleAdmin); err != nil {
		s.recordDeny(actor, OpRuleChange, "", "unauthorized")
		return Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if rule.EffectiveFrom.IsZero() {
		rule.EffectiveFrom = s.clock()
	}
	if rule.Version == 0 {
		if latest, ok := s.rules.latest(); ok {
			rule.Version = latest.Version + 1
		} else {
			rule.Version = 1
		}
	}
	if err := validateNewRule(s.rules, rule); err != nil {
		return Rule{}, err
	}
	rf, err := marshalFrame(frameRule, rulePayload{Rule: rule})
	if err != nil {
		return Rule{}, err
	}
	af, err := s.auditFrame(actor, OpRuleChange, "", rule.Version, 0,
		"rule v"+strconv.FormatInt(rule.Version, 10)+" effective "+
			rule.EffectiveFrom.Format(time.RFC3339), true)
	if err != nil {
		return Rule{}, err
	}
	lsn := s.journal.peekNext()
	rf.LSN, af.LSN = lsn, lsn+1
	if err := s.commitLocked([]journalFrame{rf, af}); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// AuditLog returns the full append-only audit trail (auditors/admins).
func (s *Service) AuditLog(actor Actor) ([]AuditRecord, error) {
	if err := actor.require(RoleAuditor, RoleAdmin); err != nil {
		s.recordDeny(actor, OpDeny, "", "audit read unauthorized")
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditRecord, len(s.audits))
	copy(out, s.audits)
	return out, nil
}

// GetPeriod returns one self-consistent view of a period.
func (s *Service) GetPeriod(actor Actor, p Period) (PeriodView, error) {
	if err := actor.require(RoleReader, RoleAuditor, RoleAdmin, RoleCloser, RoleSubmitter); err != nil {
		s.recordDeny(actor, OpDeny, p, "read unauthorized")
		return PeriodView{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pp, ok := s.periods[p]
	if !ok {
		return PeriodView{Period: p, Status: StatusOpen}, nil
	}
	v := PeriodView{
		Period:       p,
		Status:       pp.Status,
		CloseVersion: cloneVersion(pp.Current),
	}
	for _, e := range pp.Entries {
		v.EntryCount++
		v.EntryAmount += e.Amount
	}
	for _, a := range pp.Adjustments {
		v.EntryAmount += a.Amount
	}
	return v, nil
}

// cloneVersion returns a copy so callers can never mutate committed state.
func cloneVersion(v *CloseVersion) *CloseVersion {
	if v == nil {
		return nil
	}
	cp := *v
	if v.Report.Totals != nil {
		cp.Report.Totals = make(map[string]CategoryFigure, len(v.Report.Totals))
		for k, x := range v.Report.Totals {
			cp.Report.Totals[k] = x
		}
	}
	cp.EntrySeqs = append([]int64(nil), v.EntrySeqs...)
	return &cp
}
