package periodclose

import (
	"testing"
	"time"
)

func setClock(s *Service, t time.Time) {
	s.mu.Lock()
	s.clock = func() time.Time { return t }
	s.mu.Unlock()
}

func TestRuleVersionSwitchPinsClosedPeriods(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	setClock(s, t1)

	// v1: report raw, include everything.
	mustRule(t, s, Rule{Description: "v1"})

	// Period A gets entries and closes under v1.
	if err := s.SubmitEntries(submitter,
		Entry{ID: "a1", Period: "202601", Category: "sales", Amount: 1000},
		Entry{ID: "a2", Period: "202601", Category: "refund", Amount: 1000},
	); err != nil {
		t.Fatal(err)
	}
	closedA, err := s.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatal(err)
	}
	if closedA.RuleVersion != 1 || closedA.Report.Net != 2000 {
		t.Fatalf("A v1 net=%d rv=%d", closedA.Report.Net, closedA.RuleVersion)
	}
	fpA := closedA.Report.Fingerprint

	// v2: drop "refund" category and flip sign of sales. Effective at t2.
	if _, err := s.AddRule(admin, Rule{
		Description:       "v2",
		EffectiveFrom:     t2,
		IncludeCategories: []string{"sales"},
		Sign:              map[string]int64{"sales": -1},
	}); err != nil {
		t.Fatal(err)
	}

	// Still t1: a newly closed open period must still use v1.
	setClock(s, t1)
	if err := s.SubmitEntries(submitter,
		Entry{ID: "c1", Period: "202512", Category: "sales", Amount: 700},
	); err != nil {
		t.Fatal(err)
	}
	cOld, err := s.ClosePeriod(closer, "202512")
	if err != nil {
		t.Fatal(err)
	}
	if cOld.RuleVersion != 1 || cOld.Report.Net != 700 {
		t.Fatalf("period closed before t2 must pin v1: rv=%d net=%d",
			cOld.RuleVersion, cOld.Report.Net)
	}

	// At t2 an open period closes under v2.
	setClock(s, t2)
	if err := s.SubmitEntries(submitter,
		Entry{ID: "b1", Period: "202602", Category: "sales", Amount: 1000},
		Entry{ID: "b2", Period: "202602", Category: "refund", Amount: 1000},
	); err != nil {
		t.Fatal(err)
	}
	closedB, err := s.ClosePeriod(closer, "202602")
	if err != nil {
		t.Fatal(err)
	}
	if closedB.RuleVersion != 2 {
		t.Fatalf("B should close under v2, got %d", closedB.RuleVersion)
	}
	// refund excluded; sales sign flipped -> net -1000.
	if closedB.Report.Net != -1000 {
		t.Fatalf("B v2 net=%d want -1000", closedB.Report.Net)
	}
	if _, has := closedB.Report.Totals["refund"]; has {
		t.Fatalf("refund should be excluded by v2")
	}

	// A must remain exactly the v1 result despite the rule change.
	vA, _ := s.GetPeriod(reader, "202601")
	if vA.CloseVersion.RuleVersion != 1 ||
		vA.CloseVersion.Report.Fingerprint != fpA ||
		vA.CloseVersion.Report.Net != 2000 {
		t.Fatalf("closed period A shifted after rule update: %+v", vA.CloseVersion)
	}
}
