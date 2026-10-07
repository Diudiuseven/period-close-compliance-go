package periodclose

import "testing"

func TestRestartReplaysExactlyOnce(t *testing.T) {
	s, dir := testSvc(t)
	mustRule(t, s, Rule{})

	if err := s.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales", Amount: 100},
		Entry{ID: "e2", Period: "202601", Category: "sales", Amount: 200},
		Entry{ID: "e3", Period: "202602", Category: "sales", Amount: 50},
	); err != nil {
		t.Fatal(err)
	}
	cv, err := s.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatal(err)
	}
	fp := cv.Report.Fingerprint
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen from disk; replay must not double-apply entries.
	s2, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	v1, _ := s2.GetPeriod(reader, "202601")
	if v1.Status != StatusClosed || v1.CloseVersion == nil ||
		v1.CloseVersion.Report.Fingerprint != fp || v1.EntryCount != 2 {
		t.Fatalf("period1 after restart = %+v", v1)
	}
	v2, _ := s2.GetPeriod(reader, "202602")
	if v2.Status != StatusOpen || v2.EntryCount != 1 || v2.EntryAmount != 50 {
		t.Fatalf("period2 after restart = %+v", v2)
	}
	// Resubmitting an old ID after restart stays idempotent.
	if err := s2.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales", Amount: 100}); err != nil {
		t.Fatal(err)
	}
	v1b, _ := s2.GetPeriod(reader, "202601")
	if v1b.EntryCount != 2 {
		t.Fatalf("duplicate re-applied across restart: %d", v1b.EntryCount)
	}
}

func TestInterruptedCloseIsRecoverableAndDistinct(t *testing.T) {
	s, dir := testSvc(t)
	mustRule(t, s, Rule{})
	if err := s.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales", Amount: 300}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash AFTER close-start was fsynced but BEFORE commit:
	// append a raw start frame directly to the journal.
	sf, err := marshalFrame(frameCloseStart, closeStartPayload{
		Period: "202601", RuleVersion: 1, Actor: "x", At: s.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.appendBatch([]journalFrame{sf}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// The period must be distinguishable from a truly closed one: it is
	// "closing" with no committed version.
	v, _ := s2.GetPeriod(reader, "202601")
	if v.Status != StatusClosing || v.CloseVersion != nil {
		t.Fatalf("expected closing/uncommitted, got %+v", v)
	}

	// Recovery rolls it back to open; entries remain intact.
	if err := s2.Recover(admin); err != nil {
		t.Fatalf("recover: %v", err)
	}
	v2, _ := s2.GetPeriod(reader, "202601")
	if v2.Status != StatusOpen || v2.EntryAmount != 300 {
		t.Fatalf("post-recover view=%+v", v2)
	}
	// A second recovery is a no-op (nothing interrupted).
	if err := s2.Recover(admin); err != ErrNoAdvance {
		t.Fatalf("second recover err=%v", err)
	}
	// Now a normal close succeeds and yields the correct result.
	cv, err := s2.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatal(err)
	}
	if cv.Version != 1 || cv.Report.Net != 300 {
		t.Fatalf("post-recovery close = %+v", cv)
	}
}
