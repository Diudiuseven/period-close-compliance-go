package periodclose

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Service is the in-process period close & compliance engine.
//
// Concurrency model: one RWMutex serializes every operation against the
// derived in-memory state. A close holds the lock for its whole commit, so
// readers (RLock) always observe either the previous fully-open state or a
// fully committed close — never a mixed/half one. Intake for other periods
// takes the same lock but only for the brief append, so a slow period's
// computation never blocks them (compute happens before commit, under a
// per-period busy guard rather than the global lock).
type Service struct {
	mu       sync.RWMutex
	dir      string
	journal  *Journal
	rules    *ruleBook
	periods  map[Period]*persistedPeriod
	seenEnt  map[string]int64 // entry id -> global seq
	seenAdj  map[string]int64 // adjustment id -> global seq
	audits   []AuditRecord
	auditSeq int64
	lastLSN  int64
	closing  map[Period]bool // periods whose close is being computed now
	cmu      sync.Mutex
	clock    func() time.Time
}

// Config tunes a Service.
type Config struct {
	Dir string
}

// Open loads or initializes a service from dir, replaying the journal so
// all committed work survives restarts and interrupted closes are
// distinguishable from committed ones.
func Open(cfg Config) (*Service, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("%w: empty Dir", ErrBadArgument)
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	j, err := openJournal(cfg.Dir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		dir:     cfg.Dir,
		journal: j,
		rules:   newRuleBook(),
		periods: map[Period]*persistedPeriod{},
		seenEnt: map[string]int64{},
		seenAdj: map[string]int64{},
		closing: map[Period]bool{},
		clock:   time.Now,
	}
	if err := s.restore(); err != nil {
		j.close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return nil
	}
	err := s.journal.close()
	s.journal = nil
	return err
}

// ---- authorization ----

func (a Actor) require(roles ...string) error {
	for _, want := range roles {
		if a.hasRole(want) {
			return nil
		}
	}
	return ErrUnauthorized
}

// ---- persistence / recovery ----

func (s *Service) now() time.Time {
	s.mu.RLock()
	c := s.clock
	s.mu.RUnlock()
	return c()
}

// restore rebuilds derived state purely from the journal. Snapshots are an
// optimization but correctness never depends on them: a full replay yields
// the same state, which keeps results locally reproducible and makes
// restart recovery idempotent (frames never applied twice or skipped).
func (s *Service) restore() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Always replay from LSN 1. The journal is the source of truth and is
	// never pruned in this build, so this is both simple and exactly-once.
	// (Snapshots are written as a debuggable checkpoints only.)
	return s.journal.replay(func(f journalFrame) error {
		return s.applyFrame(f)
	})
}

// applyFrame folds one journal frame into in-memory state. It must be the
// exact inverse used by both live commits and crash replay (no duplicate
// application, no skipping).
func (s *Service) applyFrame(f journalFrame) error {
	switch f.Type {
	case frameEntry:
		p, err := unmarshalFrame[entryPayload](f)
		if err != nil {
			return err
		}
		s.applyEntry(p.Entry)
	case frameCloseStart:
		p, err := unmarshalFrame[closeStartPayload](f)
		if err != nil {
			return err
		}
		s.applyCloseStart(p)
	case frameCloseCommit:
		p, err := unmarshalFrame[closeCommitPayload](f)
		if err != nil {
			return err
		}
		s.applyCloseCommit(p.Version)
	case frameReopen:
		p, err := unmarshalFrame[reopenPayload](f)
		if err != nil {
			return err
		}
		s.applyReopen(p.Period)
	case frameCloseAbort:
		p, err := unmarshalFrame[closeAbortPayload](f)
		if err != nil {
			return err
		}
		s.applyAbort(p.Period)
	case frameAdjust:
		p, err := unmarshalFrame[adjustPayload](f)
		if err != nil {
			return err
		}
		s.applyAdjust(p.Adjustment)
	case frameRule:
		p, err := unmarshalFrame[rulePayload](f)
		if err != nil {
			return err
		}
		if err := s.rules.add(p.Rule); err != nil {
			return err
		}
	case frameAudit:
		p, err := unmarshalFrame[auditPayload](f)
		if err != nil {
			return err
		}
		s.audits = append(s.audits, p.Record)
		if p.Record.Seq > s.auditSeq {
			s.auditSeq = p.Record.Seq
		}
	}
	s.lastLSN = f.LSN
	return nil
}

func (s *Service) ensurePeriod(p Period) *persistedPeriod {
	pp, ok := s.periods[p]
	if !ok {
		pp = &persistedPeriod{Status: StatusOpen}
		s.periods[p] = pp
	}
	return pp
}

func (s *Service) applyEntry(e StoredEntry) {
	pp := s.ensurePeriod(e.Period)
	pp.Entries = append(pp.Entries, e)
	s.seenEnt[e.ID] = e.Seq
}

func (s *Service) applyCloseStart(p closeStartPayload) {
	pp := s.ensurePeriod(p.Period)
	pc := p
	pp.Intent = &pc
	pp.Status = StatusClosing
}

func (s *Service) applyCloseCommit(v CloseVersion) {
	pp := s.ensurePeriod(v.Period)
	vc := v
	pp.Current = &vc
	pp.History = append(pp.History, v)
	if v.Version > pp.CloseSeq {
		pp.CloseSeq = v.Version
	}
	pp.Intent = nil
	pp.Status = StatusClosed
}

func (s *Service) applyReopen(p Period) {
	pp := s.ensurePeriod(p)
	pp.Intent = nil
	pp.Status = StatusReopen
}

// applyAbort returns a period to open after an interrupted first close.
// No committed CloseVersion ever existed for that attempt.
func (s *Service) applyAbort(p Period) {
	pp := s.ensurePeriod(p)
	pp.Intent = nil
	pp.Status = StatusOpen
}

func (s *Service) applyAdjust(a Adjustment) {
	pp := s.ensurePeriod(a.Period)
	pp.Adjustments = append(pp.Adjustments, a)
	s.seenAdj[a.ID] = a.Seq
}

func (p *persistedPeriod) intentRule() int64 {
	if p.Intent != nil {
		return p.Intent.RuleVersion
	}
	return 0
}
