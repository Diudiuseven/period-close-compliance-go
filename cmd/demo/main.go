// Command demo walks one period through intake, close, a late/duplicate
// rejection, a rule change, reopen+adjustment and re-close, then reopens the
// store from disk to prove the result is locally reproducible.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Diudiuseven/period-close-compliance-go/periodclose"
)

func main() {
	dir := filepath.Join(os.TempDir(), "pcc-demo")
	_ = os.RemoveAll(dir)
	admin := periodclose.Actor{ID: "admin", Roles: []string{periodclose.RoleAdmin}}
	closer := periodclose.Actor{ID: "closer", Roles: []string{periodclose.RoleCloser}}
	sub := periodclose.Actor{ID: "sub", Roles: []string{periodclose.RoleSubmitter}}

	s, err := periodclose.Open(periodclose.Config{Dir: dir})
	must(err)

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = s.AddRule(admin, periodclose.Rule{
		Description: "v1 raw", EffectiveFrom: t1,
	})
	must(err)

	const p = periodclose.Period("202601")
	must(s.SubmitEntries(sub,
		periodclose.Entry{ID: "e1", Period: p, Category: "sales", Amount: 1000},
		periodclose.Entry{ID: "e2", Period: p, Category: "sales", Amount: 250},
		periodclose.Entry{ID: "e1", Period: p, Category: "sales", Amount: 1000}, // duplicate
	))
	v1, err := s.ClosePeriod(closer, p)
	must(err)
	fmt.Printf("first close : version=%d rule=v%d net=%d fp=%s\n",
		v1.Version, v1.RuleVersion, v1.Report.Net, v1.Report.Fingerprint[:12])

	// Late entry after close cannot move the result.
	must(s.SubmitEntries(sub,
		periodclose.Entry{ID: "late", Period: p, Category: "sales", Amount: 9999}))
	view, _ := s.GetPeriod(admin, p)
	fmt.Printf("after late  : status=%s net stays=%d\n",
		view.Status, view.CloseVersion.Report.Net)

	// Reopen, append correction, re-close.
	must(s.ReopenPeriod(admin, p, "missing invoice discovered"))
	must(s.AddAdjustment(admin, periodclose.Adjustment{
		ID: "adj1", Period: p, Category: "sales", Amount: 150, Reason: "invoice #42",
	}))
	v2, err := s.ClosePeriod(closer, p)
	must(err)
	fmt.Printf("re-close    : version=%d rule=v%d net=%d fp=%s\n",
		v2.Version, v2.RuleVersion, v2.Report.Net, v2.Report.Fingerprint[:12])
	must(s.Close())

	// Reopen from disk: identical fingerprint => locally reproducible.
	s2, err := periodclose.Open(periodclose.Config{Dir: dir})
	must(err)
	after, err := s2.GetPeriod(admin, p)
	must(err)
	fmt.Printf("after restart: status=%s version=%d net=%d fp=%s (match=%v)\n",
		after.Status, after.CloseVersion.Version, after.CloseVersion.Report.Net,
		after.CloseVersion.Report.Fingerprint[:12],
		after.CloseVersion.Report.Fingerprint == v2.Report.Fingerprint)

	recs, _ := s2.AuditLog(admin)
	fmt.Printf("audit records persisted: %d\n", len(recs))
	must(s2.Close())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
