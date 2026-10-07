package compliance

import (
	"context"
	"testing"
	"time"
)

func testSvc(t *testing.T) *Service {
	t.Helper()
	cfg := InMemoryConfig()
	cfg.DefaultRules = []Rule{{
		Version: 1, Name: "test", EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints: 0, DeductibleCategories: []string{"cost"},
	}}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func t0() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) }

func TestIngestDuplicateLateOutOfOrder(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")

	entries := []Entry{
		{IdempotencyKey: "e3", Period: p, Category: "revenue", Amount: 300, OccurredAt: t0().Add(3 * time.Hour)},
		{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 1000, OccurredAt: t0()},
		{IdempotencyKey: "e2", Period: p, Category: "cost", Amount: 400, OccurredAt: t0().Add(2 * time.Hour)},
		// 与 e1 完全相同的重复提交
		{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 1000, OccurredAt: t0()},
		// 同键但金额不同的重复提交：仍必须拒绝，不得重复计入
		{IdempotencyKey: "e2", Period: p, Category: "cost", Amount: 9999, OccurredAt: t0()},
	}
	for i, e := range entries {
		err := svc.Ingest(ctx, admin, e)
		if i >= 3 {
			if err != ErrDuplicateEntry {
				t.Fatalf("entry %d: want ErrDuplicateEntry got %v", i, err)
			}
		} else if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}

	v, err := svc.ClosePeriod(ctx, admin, p, t0().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	// 只计入 e1/e2/e3 各一次，且按发生时间排序后确定唯一
	if got := v.Balances["revenue"]; got != 1300 {
		t.Fatalf("revenue = %d, want 1300", got)
	}
	if got := v.Balances["cost"]; got != 400 {
		t.Fatalf("cost = %d, want 400", got)
	}
	if len(v.EntryKeys) != 3 || v.EntryKeys[0] != "e1" || v.EntryKeys[1] != "e2" || v.EntryKeys[2] != "e3" {
		t.Fatalf("entry keys order not deterministic: %v", v.EntryKeys)
	}

	// 迟到明细：结账后到达，不得偏移已结账结果
	err = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "late1", Period: p, Category: "revenue", Amount: 5000, OccurredAt: t0()})
	if err != ErrPeriodClosed {
		t.Fatalf("late entry: want ErrPeriodClosed got %v", err)
	}
	view, err := svc.GetPeriod(ctx, admin, p)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusClosed || view.Current.Filing.Gross != 1300 {
		t.Fatalf("closed result drifted: %+v", view.Current.Filing)
	}
}

func TestCloseIdempotentUniqueness(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")
	_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 500, OccurredAt: t0()})

	v1, err := svc.ClosePeriod(ctx, admin, p, t0())
	if err != nil {
		t.Fatal(err)
	}
	// 重复结账必须被拒绝
	if _, err := svc.ClosePeriod(ctx, admin, p, t0()); err != ErrPeriodClosed {
		t.Fatalf("second close: want ErrPeriodClosed got %v", err)
	}
	view, _ := svc.GetPeriod(ctx, admin, p)
	if view.Current.Version != v1.Version || view.Current.ClosedAt != v1.ClosedAt {
		t.Fatal("closed version must stay uniquely fixed")
	}
}
