package compliance

import (
	"context"
	"testing"
	"time"
)

func TestReopenAdjustmentAlignsWithFullRecompute(t *testing.T) {
	svc := ruleSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")

	ing := func(k, cat string, amt int64) {
		t.Helper()
		if err := svc.Ingest(ctx, admin, Entry{
			IdempotencyKey: k, Period: p, Category: cat, Amount: amt,
			OccurredAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ing("e1", "revenue", 10000)
	ing("e2", "cost", 2000)

	v1, err := svc.ClosePeriod(ctx, admin, p, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.Filing.TaxDue != 800 {
		t.Fatalf("v1: %+v", v1.Filing)
	}

	// 前置条件：只有已结账期间可重开；未结账期间重开应失败
	if err := svc.Reopen(ctx, admin, "2030-01", "x"); err != ErrNotFound {
		t.Fatalf("reopen unknown: want ErrNotFound got %v", err)
	}

	if err := svc.Reopen(ctx, admin, p, "late invoice + correction"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	view, _ := svc.GetPeriod(ctx, admin, p)
	if view.Status != StatusReopened {
		t.Fatalf("status=%s want reopened", view.Status)
	}
	// 重开后普通明细不得进入，只能追加更正
	if err := svc.Ingest(ctx, admin, Entry{IdempotencyKey: "x", Period: p, Category: "revenue", Amount: 1, OccurredAt: time.Now()}); err != ErrNotReopened {
		t.Fatalf("ingest on reopened: want ErrNotReopened got %v", err)
	}
	a, err := svc.AppendAdjustment(ctx, admin, Adjustment{
		Period: p, Category: "cost", Amount: 1000, Reason: "late invoice",
	})
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if a.ID == "" || a.Author != "a" {
		t.Fatalf("adjustment not stamped: %+v", a)
	}
	// 调整 ID 幂等：重复 ID 拒绝
	if _, err := svc.AppendAdjustment(ctx, admin, Adjustment{ID: a.ID, Period: p, Category: "cost", Amount: 1}); err != ErrDuplicateEntry {
		t.Fatalf("dup adjustment: want ErrDuplicateEntry got %v", err)
	}
	// 未重开期间不能追加调整
	if _, err := svc.AppendAdjustment(ctx, admin, Adjustment{Period: "2030-02", Amount: 1}); err != ErrNotFound {
		t.Fatalf("adjust unknown: want ErrNotFound got %v", err)
	}

	// 重新结账：结果必须与"按全量口径（原始明细+全部追加更正）重算"一致
	v2, err := svc.ClosePeriod(ctx, admin, p, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 version = %d want 2", v2.Version)
	}
	wantBal := Balances{"revenue": 10000, "cost": 3000}
	wantFiling := applyFiling(mustRule(svc, v2.RuleVersion), wantBal)
	if !filingEqual(v2.Filing, wantFiling) {
		t.Fatalf("reclose filing %+v != full recompute %+v", v2.Filing, wantFiling)
	}
	if len(v2.AdjustmentIDs) != 1 || v2.AdjustmentIDs[0] != a.ID {
		t.Fatalf("adjustments not in version: %v", v2.AdjustmentIDs)
	}
	// 原始条目仍全部在版本中（追加式，不改写历史）
	if len(v2.EntryKeys) != 2 {
		t.Fatalf("original entries lost: %v", v2.EntryKeys)
	}
	// 历史版本保留且不可变；v1 结果不变
	view, _ = svc.GetPeriod(ctx, admin, p)
	if view.Status != StatusClosed || len(view.History) != 2 {
		t.Fatalf("after reclose: status=%s history=%d", view.Status, len(view.History))
	}
	h1 := view.History[0]
	if h1.Filing.TaxDue != 800 || h1.Version != 1 {
		t.Fatalf("historical version mutated: %+v", h1.Filing)
	}
	// 再次结账必须拒绝
	if _, err := svc.ClosePeriod(ctx, admin, p, time.Now()); err != ErrPeriodClosed {
		t.Fatalf("double close: want ErrPeriodClosed got %v", err)
	}
}

func mustRule(svc *Service, v int) Rule {
	r, err := svc.rules.Version(v)
	if err != nil {
		panic(err)
	}
	return r
}

func filingEqual(a, b FilingResult) bool {
	if a.Gross != b.Gross || a.Deductible != b.Deductible || a.Taxable != b.Taxable ||
		a.TaxDue != b.TaxDue || a.RuleVersion != b.RuleVersion || len(a.ByCategory) != len(b.ByCategory) {
		return false
	}
	for k, v := range a.ByCategory {
		if b.ByCategory[k] != v {
			return false
		}
	}
	return true
}
