package periodclose

import "testing"

func TestReopenAdjustRecloseAligns(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	if err := s.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales", Amount: 1000},
	); err != nil {
		t.Fatal(err)
	}
	v1, err := s.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.Report.Net != 1000 {
		t.Fatalf("v1 = %+v", v1)
	}

	// Reopen requires admin: closer cannot.
	if err := s.ReopenPeriod(closer, "202601", "try"); err != ErrUnauthorized {
		t.Fatalf("closer reopen err=%v", err)
	}
	// Cannot adjust a period that is merely closed.
	if err := s.AddAdjustment(admin, Adjustment{ID: "a0", Period: "202601",
		Category: "sales", Amount: 100}); err != ErrNotClosed {
		t.Fatalf("adjust before reopen err=%v", err)
	}

	if err := s.ReopenPeriod(admin, "202601", "book correction"); err != nil {
		t.Fatal(err)
	}
	view, _ := s.GetPeriod(reader, "202601")
	if view.Status != StatusReopen {
		t.Fatalf("status=%s", view.Status)
	}
	// Cannot reopen twice / reopen an open period.
	if err := s.ReopenPeriod(admin, "202601", "again"); err != ErrNotClosed {
		t.Fatalf("second reopen err=%v", err)
	}

	// Append-only corrections; duplicates by id are idempotent.
	adj := Adjustment{ID: "adj1", Period: "202601", Category: "sales",
		Amount: 250, Reason: "missing invoice"}
	if err := s.AddAdjustment(admin, adj); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAdjustment(admin, adj); err != nil {
		t.Fatalf("duplicate adjustment should be idempotent: %v", err)
	}

	v2, err := s.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("reclose version=%d want 2", v2.Version)
	}
	if v2.Report.Net != 1250 {
		t.Fatalf("reclose net=%d want 1250", v2.Report.Net)
	}

	// Alignment with a full from-scratch recomputation under the same rule.
	rule, _ := s.rules.get(v2.RuleVersion)
	pp := s.periods["202601"]
	full := recalc("202601", cloneEntries(pp.Entries), cloneAdjusts(pp.Adjustments), rule)
	if full.Fingerprint != v2.Report.Fingerprint || full.Net != v2.Report.Net {
		t.Fatalf("reclose does not align with full recompute:\n reclose=%s net=%d\n full=%s net=%d",
			v2.Report.Fingerprint, v2.Report.Net, full.Fingerprint, full.Net)
	}

	// History keeps both versions for explainability.
	if len(pp.History) != 2 || pp.History[0].Report.Net != 1000 {
		t.Fatalf("history = %+v", pp.History)
	}
}
