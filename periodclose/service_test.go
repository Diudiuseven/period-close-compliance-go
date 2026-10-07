package periodclose

import (
	"path/filepath"
	"testing"
	"time"
)

var (
	admin     = Actor{ID: "admin1", Roles: []string{RoleAdmin}}
	closer    = Actor{ID: "closer1", Roles: []string{RoleCloser}}
	submitter = Actor{ID: "sub1", Roles: []string{RoleSubmitter}}
	auditor   = Actor{ID: "aud1", Roles: []string{RoleAuditor}}
	reader    = Actor{ID: "ro1", Roles: []string{RoleReader}}
	nobody    = Actor{ID: "nob", Roles: nil}
)

func testSvc(t *testing.T) (*Service, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, dir
}

func mustRule(t *testing.T, s *Service, r Rule) Rule {
	t.Helper()
	if r.EffectiveFrom.IsZero() {
		r.EffectiveFrom = time.Unix(1000, 0)
	}
	got, err := s.AddRule(admin, r)
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}
	return got
}

func TestSmokeCloseAndRead(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{Description: "base", CategoryMap: map[string]string{
		"sales_a": "sales", "sales_b": "sales",
	}})

	if err := s.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales_a", Amount: 1000},
		Entry{ID: "e2", Period: "202601", Category: "sales_b", Amount: 250},
		Entry{ID: "e3", Period: "202601", Category: "tax", Amount: -100},
	); err != nil {
		t.Fatalf("submit: %v", err)
	}

	cv, err := s.ClosePeriod(closer, "202601")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if cv.Report.Net != 1150 {
		t.Fatalf("net = %d want 1150; totals=%v", cv.Report.Net, cv.Report.Totals)
	}
	if got := cv.Report.Totals["sales"].Amount; got != 1250 {
		t.Fatalf("sales = %d want 1250", got)
	}

	v, err := s.GetPeriod(reader, "202601")
	if err != nil || v.Status != StatusClosed || v.CloseVersion == nil {
		t.Fatalf("view = %+v err=%v", v, err)
	}
}
