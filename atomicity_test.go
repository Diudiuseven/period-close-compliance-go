package compliance

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 结账与读取并发进行：读取方任何时刻只能看到 "无版本" 或某个完整版本，
// 绝不能看到字段级混合 / 结账到一半的结果。
func TestReaderNeverSeesMixedVersion(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-05")
	for i := 0; i < 100; i++ {
		_ = svc.Ingest(ctx, admin, Entry{
			IdempotencyKey: "e" + strconv.Itoa(i), Period: p, Category: "revenue",
			Amount: 100, OccurredAt: t0(),
		})
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				v, err := svc.GetPeriod(ctx, admin, p)
				if err == nil && v.Current != nil {
					assertVersionSelfConsistent(t, svc, v.Current)
				}
			}
		}
	}()

	if _, err := svc.ClosePeriod(ctx, admin, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	// 再走一轮 重开->调整->重结，读取同样必须始终自洽
	if err := svc.Reopen(ctx, admin, p, "adj"); err != nil {
		t.Fatal(err)
	}
	stop2 := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop2:
				return
			default:
				v, _ := svc.GetPeriod(ctx, admin, p)
				if v != nil && v.Current != nil {
					assertVersionSelfConsistent(t, svc, v.Current)
				}
			}
		}
	}()
	if _, err := svc.AppendAdjustment(ctx, admin, Adjustment{Period: p, Category: "cost", Amount: 50, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, admin, p, time.Now()); err != nil {
		t.Fatal(err)
	}
	close(stop2)
	wg.Wait()

	view, _ := svc.GetPeriod(ctx, admin, p)
	assertVersionSelfConsistent(t, svc, view.Current)
	if view.Current.Version != 2 {
		t.Fatalf("final version=%d want 2", view.Current.Version)
	}
}

func assertVersionSelfConsistent(t *testing.T, svc *Service, cv *CloseVersion) {
	t.Helper()
	// 完整版本内部一致性：Filing 必须能由 Balances 与版本固定的规则重算得到。
	r, err := svc.rules.Version(cv.RuleVersion)
	if err != nil {
		t.Errorf("closed version references missing rule v%d", cv.RuleVersion)
		return
	}
	want := applyFiling(r, cv.Balances)
	if !filingEqual(cv.Filing, want) {
		t.Errorf("mixed/inconsistent version: stored=%+v recomputed=%+v", cv.Filing, want)
	}
	if cv.ClosedAt.IsZero() || cv.ClosedBy == "" {
		t.Errorf("partially written version metadata: %+v", cv)
	}
}
