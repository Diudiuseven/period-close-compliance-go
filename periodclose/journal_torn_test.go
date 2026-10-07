package periodclose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTornTrailingFrameIsTruncatedOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	s, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	mustRule(t, s, Rule{})
	if err := s.SubmitEntries(submitter,
		Entry{ID: "good", Period: "202601", Category: "c", Amount: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Append garbage bytes to simulate a torn partial write at crash.
	p := filepath.Join(dir, journalName)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x5a, 0x4c, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x99}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen with torn tail: %v", err)
	}
	defer s2.Close()
	v, _ := s2.GetPeriod(reader, "202601")
	if v.EntryCount != 1 || v.EntryAmount != 7 {
		t.Fatalf("torn tail corrupted state: %+v", v)
	}
	// Appending after recovery works (truncated tail did not break LSN contiguity).
	if err := s2.SubmitEntries(submitter,
		Entry{ID: "after", Period: "202601", Category: "c", Amount: 3}); err != nil {
		t.Fatal(err)
	}
	v2, _ := s2.GetPeriod(reader, "202601")
	if v2.EntryCount != 2 || v2.EntryAmount != 10 {
		t.Fatalf("post-torn append wrong: %+v", v2)
	}
}
