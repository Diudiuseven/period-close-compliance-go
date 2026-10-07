package periodclose

import "testing"

func TestAuditTrailCoversAllOperations(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	_ = s.SubmitEntries(submitter, Entry{ID: "e1", Period: "202601", Category: "c", Amount: 10})
	_, _ = s.ClosePeriod(closer, "202601")
	_ = s.ReopenPeriod(admin, "202601", "fix")
	_ = s.AddAdjustment(admin, Adjustment{ID: "adj1", Period: "202601",
		Category: "c", Amount: 5, Reason: "r"})
	_, _ = s.ClosePeriod(closer, "202601")
	_, _ = s.AddRule(admin, Rule{})
	// Denied actions across all sensitive ops.
	_ = s.ReopenPeriod(reader, "202601", "x")
	_ = s.AddAdjustment(reader, Adjustment{ID: "z", Period: "202601"})
	_, _ = s.ClosePeriod(reader, "202601")

	recs, err := s.AuditLog(auditor)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[OpKind]int{}
	var lastSeq int64 = -1
	for _, r := range recs {
		counts[r.Op]++
		if r.Seq <= lastSeq {
			t.Fatalf("audit seq not strictly increasing: %d after %d", r.Seq, lastSeq)
		}
		lastSeq = r.Seq
		if r.At.IsZero() || r.ActorID == "" {
			t.Fatalf("incomplete audit record: %+v", r)
		}
	}
	for _, op := range []OpKind{OpCloseStart, OpCloseCommit, OpReopen, OpAdjust, OpRuleChange, OpDeny} {
		if counts[op] == 0 {
			t.Fatalf("missing audit records for %s (counts=%v)", op, counts)
		}
	}
	// Auditor must not be able to mutate; reader must not read the audit log.
	if _, err := s.AuditLog(reader); err != ErrUnauthorized {
		t.Fatalf("reader audit access err=%v", err)
	}
}
