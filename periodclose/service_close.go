package periodclose

import (
	"strconv"
)

// tryAcquireClose marks a period as being computed. The expensive report
// calculation happens while the global state lock is NOT held, so a slow
// period cannot block intake or closing of other periods.
func (s *Service) tryAcquireClose(p Period) bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.closing[p] {
		return false
	}
	s.closing[p] = true
	return true
}

func (s *Service) releaseClose(p Period) {
	s.cmu.Lock()
	s.closing[p] = false
	s.cmu.Unlock()
}

// ClosePeriod atomically closes a period.
//
// It is a two-phase commit:
//   - phase 1 (locked): snapshot the exact accepted line set, choose the
//     currently effective rule, durably write a close-start intent and set
//     status to "closing";
//   - compute (unlocked): deterministic report from the captured inputs;
//   - phase 2 (locked): verify the period is still the same intent, then
//     durably commit the immutable CloseVersion and publish it atomically.
//
// Readers therefore see either the previous open state or the full new
// closed version, never a half-written/mixed result.
func (s *Service) ClosePeriod(actor Actor, p Period) (*CloseVersion, error) {
	if err := actor.require(RoleCloser, RoleAdmin); err != nil {
		s.recordDeny(actor, OpCloseCommit, p, "unauthorized")
		return nil, err
	}
	if p == "" {
		return nil, ErrBadArgument
	}
	if !s.tryAcquireClose(p) {
		return nil, ErrClosing
	}
	defer s.releaseClose(p)

	// ---- phase 1: capture inputs + rule, persist intent ----
	s.mu.Lock()
	pp := s.ensurePeriod(p)
	if pp.Status == StatusClosed {
		s.mu.Unlock()
		return nil, ErrPeriodClosed
	}
	if pp.Status == StatusClosing && pp.Intent != nil {
		s.mu.Unlock()
		return nil, ErrClosing
	}
	rule, ok := s.rules.effectiveAt(s.clock())
	if !ok {
		s.mu.Unlock()
		return nil, ErrRuleNotFound
	}

	fromVersion := pp.CloseSeq // 0 for first close
	entries := cloneEntries(pp.Entries)
	adjusts := cloneAdjusts(pp.Adjustments)
	intent := closeStartPayload{
		Period:      p,
		RuleVersion: rule.Version,
		Actor:       actor.ID,
		At:          s.clock(),
	}
	sf, err := marshalFrame(frameCloseStart, intent)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	af, err := s.auditFrame(actor, OpCloseStart, p, rule.Version, fromVersion,
		"close started (becomes v"+strconv.FormatInt(fromVersion+1, 10)+")", true)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	lsn := s.journal.peekNext()
	sf.LSN, af.LSN = lsn, lsn+1
	if err := s.commitLocked([]journalFrame{sf, af}); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	// ---- compute outside the global lock: other periods keep moving ----
	report := recalc(p, entries, adjusts, rule)
	seqs := make([]int64, 0, len(entries)+len(adjusts))
	for _, e := range entries {
		seqs = append(seqs, e.Seq)
	}
	for _, a := range adjusts {
		seqs = append(seqs, a.Seq)
	}

	// ---- phase 2: commit immutable version ----
	s.mu.Lock()
	defer s.mu.Unlock()

	pp2 := s.ensurePeriod(p)
	// Guard against a concurrent reopen/reset that superseded our intent.
	if pp2.Intent == nil || pp2.Status != StatusClosing ||
		pp2.Intent.RuleVersion != rule.Version {
		return nil, ErrClosing
	}
	newVer := pp2.CloseSeq + 1
	report.GeneratedAt = s.clock()
	cv := CloseVersion{
		Period:      p,
		Version:     newVer,
		RuleVersion: rule.Version,
		ClosedAt:    s.clock(),
		EntrySeqs:   seqs,
		Report:      report,
	}

	// Alignment check: recompute from the FULL current accepted lines under
	// the pinned rule and require an identical fingerprint. Re-closes after
	// adjustment must equal a from-scratch recomputation.
	full := recalc(p, cloneEntries(pp2.Entries), cloneAdjusts(pp2.Adjustments), rule)
	if full.Fingerprint != report.Fingerprint {
		return nil, errString("periodclose: close alignment check failed")
	}

	cf, err := marshalFrame(frameCloseCommit, closeCommitPayload{Version: cv})
	if err != nil {
		return nil, err
	}
	ca, err := s.auditFrame(actor, OpCloseCommit, p, rule.Version, newVer,
		"close committed v"+strconv.FormatInt(newVer, 10)+
			" net="+strconv.FormatInt(report.Net, 10)+
			" fp="+report.Fingerprint[:12], true)
	if err != nil {
		return nil, err
	}
	lsn2 := s.journal.peekNext()
	cf.LSN, ca.LSN = lsn2, lsn2+1
	if err := s.commitLocked([]journalFrame{cf, ca}); err != nil {
		return nil, err
	}
	return cloneVersion(pp2.Current), nil
}

// ReopenPeriod reopens a closed period so corrections can be appended.
// Preconditions: the period must currently be closed; the actor must be an
// admin. Reopen is itself a durable, audited transition; the previous
// CloseVersion is retained in history and remains explainable.
func (s *Service) ReopenPeriod(actor Actor, p Period, reason string) error {
	if err := actor.require(RoleAdmin); err != nil {
		s.recordDeny(actor, OpReopen, p, "unauthorized")
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pp := s.ensurePeriod(p)
	if pp.Status != StatusClosed || pp.Current == nil {
		return ErrNotClosed
	}
	from := pp.Current.Version
	rp := reopenPayload{
		Period:      p,
		FromVersion: from,
		Actor:       actor.ID,
		Reason:      reason,
		At:          s.clock(),
	}
	rf, err := marshalFrame(frameReopen, rp)
	if err != nil {
		return err
	}
	af, err := s.auditFrame(actor, OpReopen, p, pp.Current.RuleVersion, from,
		"reopened from v"+strconv.FormatInt(from, 10)+": "+reason, true)
	if err != nil {
		return err
	}
	lsn := s.journal.peekNext()
	rf.LSN, af.LSN = lsn, lsn+1
	return s.commitLocked([]journalFrame{rf, af})
}

// AddAdjustment appends a correction to a reopened period. Adjustments are
// append-only: they never edit or delete existing lines. Idempotent by ID.
func (s *Service) AddAdjustment(actor Actor, a Adjustment) error {
	if err := actor.require(RoleAdmin); err != nil {
		s.recordDeny(actor, OpAdjust, a.Period, "unauthorized")
		return err
	}
	if a.ID == "" || a.Period == "" {
		return ErrBadArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.seenAdj[a.ID]; dup {
		return nil // idempotent
	}
	pp := s.ensurePeriod(a.Period)
	if pp.Status != StatusReopen {
		return ErrNotClosed // adjustments require a reopened period
	}
	a.Seq = s.journal.peekNext()
	a.CreatedAt = s.clock()
	jf, err := marshalFrame(frameAdjust, adjustPayload{Adjustment: a})
	if err != nil {
		return err
	}
	ruleVer := int64(0)
	if pp.Current != nil {
		ruleVer = pp.Current.RuleVersion
	}
	af, err := s.auditFrame(actor, OpAdjust, a.Period, ruleVer, pp.CloseSeq,
		"adjustment "+a.ID+" amount="+strconv.FormatInt(a.Amount, 10)+
			" reason="+a.Reason, true)
	if err != nil {
		return err
	}
	jf.LSN = a.Seq
	af.LSN = a.Seq + 1
	return s.commitLocked([]journalFrame{jf, af})
}

// Recover explicitly resolves periods left in the transient "closing"
// state by a crash mid-close. No CloseVersion was committed for them, so
// they are returned to "open" deterministically and can be closed again.
// Committed (closed) periods are untouched, keeping the two states
// distinguishable after restart. Admin only; every resolution is audited.
func (s *Service) Recover(actor Actor) error {
	if err := actor.require(RoleAdmin); err != nil {
		s.recordDeny(actor, OpCloseStart, "", "recover unauthorized")
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	frames := make([]journalFrame, 0)
	lsn := s.journal.peekNext()
	for p, pp := range s.periods {
		if pp.Status != StatusClosing {
			continue
		}
		// A closing period has no committed version; roll it back to open.
		ab := closeAbortPayload{Period: p, Actor: actor.ID,
			Reason: "crash recovery: interrupted close rolled back", At: s.clock()}
		rf, err := marshalFrame(frameCloseAbort, ab)
		if err != nil {
			return err
		}
		af, err := s.auditFrame(actor, OpCloseStart, p, pp.intentRule(),
			pp.CloseSeq, "recovered interrupted close -> open", true)
		if err != nil {
			return err
		}
		rf.LSN, af.LSN = lsn, lsn+1
		lsn += 2
		frames = append(frames, rf, af)
	}
	if len(frames) == 0 {
		return ErrNoAdvance
	}
	return s.commitLocked(frames)
}

// ---- defensive copiers ----

func cloneEntries(in []StoredEntry) []StoredEntry {
	out := make([]StoredEntry, len(in))
	copy(out, in)
	return out
}

func cloneAdjusts(in []Adjustment) []Adjustment {
	out := make([]Adjustment, len(in))
	copy(out, in)
	return out
}
