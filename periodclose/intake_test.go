package periodclose

import "testing"

// Duplicate resubmissions must be counted exactly once even when the same
// batch repeats the ID, and later batches repeat it again.
func TestDuplicateEntriesIdempotent(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	e := Entry{ID: "dup1", Period: "202602", Category: "c", Amount: 500}
	// Same ID repeated inside one batch and across batches.
	if err := s.SubmitEntries(submitter, e, e,
		Entry{ID: "x", Period: "202602", Category: "c", Amount: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitEntries(submitter, e); err != nil {
		t.Fatal(err)
	}

	v, _ := s.GetPeriod(reader, "202602")
	if v.EntryCount != 2 || v.EntryAmount != 600 {
		t.Fatalf("count=%d amount=%d want 2/600", v.EntryCount, v.EntryAmount)
	}
}

// Late arrivals (after close) and out-of-order arrivals must not move the
// closed result, and the period stays closed with the same fingerprint.
func TestLateAndOutOfOrderDoNotShiftClose(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	// Out-of-order: logical order differs from arrival order.
	if err := s.SubmitEntries(submitter,
		Entry{ID: "b", Period: "202603", Category: "c", Amount: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitEntries(submitter,
		Entry{ID: "a", Period: "202603", Category: "c", Amount: 100}); err != nil {
		t.Fatal(err)
	}
	cv1, err := s.ClosePeriod(closer, "202603")
	if err != nil {
		t.Fatal(err)
	}
	if cv1.Report.Net != 300 {
		t.Fatalf("net=%d want 300", cv1.Report.Net)
	}

	// Late entry for the closed period.
	if err := s.SubmitEntries(submitter,
		Entry{ID: "late1", Period: "202603", Category: "c", Amount: 9999}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetPeriod(reader, "202603")
	if v.Status != StatusClosed || v.CloseVersion == nil {
		t.Fatalf("status=%s", v.Status)
	}
	if v.CloseVersion.Report.Fingerprint != cv1.Report.Fingerprint ||
		v.CloseVersion.Report.Net != 300 {
		t.Fatalf("closed result shifted: %+v", v.CloseVersion.Report)
	}
	if v.EntryCount != 2 {
		t.Fatalf("late entry leaked into period: count=%d", v.EntryCount)
	}
}

// An unauthorized submitter is rejected and the attempt is auditable.
func TestIntakeUnauthorizedDeniedAndAudited(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	err := s.SubmitEntries(nobody, Entry{ID: "z", Period: "202604", Category: "c", Amount: 1})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v want ErrUnauthorized", err)
	}
	recs, err := s.AuditLog(auditor)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range recs {
		if r.Op == OpDeny && !r.Allowed && r.ActorID == "nob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied intake not recorded: %+v", recs)
	}
}
