package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Diudiuseven/period-close-compliance-go"
)

func main() {
	ctx := context.Background()
	dir := "./.demo-data"
	cfg := compliance.DefaultConfig()
	cfg.DataDir = dir
	cfg.DefaultRules = []compliance.Rule{{
		Version: 1, Name: "v1 10% tax", EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints:   1000,
		DeductibleCategories: []string{"cost"},
		PublishedBy:          "system", PublishedAt: time.Now().UTC(),
	}}

	svc, err := compliance.New(cfg)
	must(err)
	admin := compliance.NewAdmin("alice")
	p := compliance.Period("2026-09")
	t0 := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	must(svc.Ingest(ctx, admin, compliance.Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 10000, OccurredAt: t0}))
	must(svc.Ingest(ctx, admin, compliance.Entry{IdempotencyKey: "e2", Period: p, Category: "cost", Amount: 3000, OccurredAt: t0.Add(time.Hour)}))
	// 重复提交
	if err := svc.Ingest(ctx, admin, compliance.Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 10000, OccurredAt: t0}); err != compliance.ErrDuplicateEntry {
		panic("expected duplicate error")
	}

	v1, err := svc.ClosePeriod(ctx, admin, p, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	must(err)
	fmt.Printf("close v1: gross=%d deductible=%d taxDue=%d rule=v%d\n", v1.Filing.Gross, v1.Filing.Deductible, v1.Filing.TaxDue, v1.Filing.RuleVersion)

	// 迟到明细：结账后拒绝
	if err := svc.Ingest(ctx, admin, compliance.Entry{IdempotencyKey: "e3", Period: p, Category: "revenue", Amount: 5000, OccurredAt: t0}); err != compliance.ErrPeriodClosed {
		panic("expected closed error")
	}

	// 发布新规则（20%），只影响之后的结账
	must(svc.PublishRule(ctx, admin, compliance.Rule{Version: 2, Name: "v2 20% tax", EffectiveAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), TaxRateBasisPoints: 2000, DeductibleCategories: []string{"cost"}}))

	// 重开、追加调整、重新结账
	must(svc.Reopen(ctx, admin, p, "missed cost invoice"))
	_, err = svc.AppendAdjustment(ctx, admin, compliance.Adjustment{Period: p, Category: "cost", Amount: 1000, Reason: "late invoice"})
	must(err)
	v2, err := svc.ClosePeriod(ctx, admin, p, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	must(err)
	fmt.Printf("close v2: gross=%d deductible=%d taxDue=%d rule=v%d history=%d\n", v2.Filing.Gross, v2.Filing.Deductible, v2.Filing.TaxDue, v2.Filing.RuleVersion, 2)
	must(svc.Close())

	// 重启：从本地 WAL 恢复，结果必须一致
	svc2, err := compliance.New(cfg)
	must(err)
	view, err := svc2.GetPeriod(ctx, admin, p)
	must(err)
	fmt.Printf("restart: status=%s currentVersion=v%d taxDue=%d\n", view.Status, view.Current.Version, view.Current.Filing.TaxDue)
	evs, err := svc2.AuditEvents(ctx, admin, 0)
	must(err)
	fmt.Printf("audit events: %d\n", len(evs))
	must(svc2.Close())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
