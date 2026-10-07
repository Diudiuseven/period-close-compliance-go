package compliance

import (
	"context"
	"testing"
	"time"
)

func publishTestRules(t *testing.T, svc *Service, admin User) {
	t.Helper()
	ctx := context.Background()
	must(t, svc.PublishRule(ctx, admin, Rule{
		Version: 2, Name: "20%", EffectiveAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints: 2000, DeductibleCategories: []string{"cost"},
	}))
}

func ruleSvc(t *testing.T) *Service {
	t.Helper()
	cfg := InMemoryConfig()
	cfg.DefaultRules = []Rule{{
		Version: 1, Name: "10%", EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints: 1000, DeductibleCategories: []string{"cost"},
	}}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestRuleVersionSwitching(t *testing.T) {
	svc := ruleSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	publishTestRules(t, svc, admin)

	rev := Period("2026-09")
	new := Period("2026-10")
	ing := func(p Period, k string, cat string, amt int64, day int) {
		t.Helper()
		month := time.Month(int(p[5]-'0')*10 + int(p[6]-'0'))
		if err := svc.Ingest(ctx, admin, Entry{
			IdempotencyKey: k, Period: p, Category: cat, Amount: amt,
			OccurredAt: time.Date(2026, month, day, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ing(rev, "r1", "revenue", 10000, 5)
	ing(rev, "c1", "cost", 2000, 6)
	ing(new, "r2", "revenue", 10000, 5)
	ing(new, "c2", "cost", 2000, 6)

	// 9 月在 10-01 前结账：用 v1（10%）
	v9, err := svc.ClosePeriod(ctx, admin, rev, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if v9.RuleVersion != 1 || v9.Filing.TaxDue != 800 {
		t.Fatalf("sep close: rule=%d tax=%d want rule=1 tax=800", v9.RuleVersion, v9.Filing.TaxDue)
	}

	// 发布 v3（30%）在 10-15 生效
	if err := svc.PublishRule(ctx, admin, Rule{
		Version: 3, Name: "30%", EffectiveAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints: 3000, DeductibleCategories: []string{"cost"},
	}); err != nil {
		t.Fatal(err)
	}

	// 10 月在 10-31 结账：按最新生效规则 v3；已结账的 9 月结果必须不变
	v10, err := svc.ClosePeriod(ctx, admin, new, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if v10.RuleVersion != 3 || v10.Filing.TaxDue != 2400 {
		t.Fatalf("oct close: rule=%d tax=%d want rule=3 tax=2400", v10.RuleVersion, v10.Filing.TaxDue)
	}
	view9, err := svc.GetPeriod(ctx, admin, rev)
	if err != nil {
		t.Fatal(err)
	}
	if view9.Current.RuleVersion != 1 || view9.Current.Filing.TaxDue != 800 {
		t.Fatalf("closed period drifted after rule change: rule=%d tax=%d",
			view9.Current.RuleVersion, view9.Current.Filing.TaxDue)
	}

	// 规则不可变/版本号唯一
	if err := svc.PublishRule(ctx, admin, Rule{Version: 2, Name: "x"}); err != ErrRuleExists {
		t.Fatalf("republish v2: want ErrRuleExists got %v", err)
	}
	// 指定时刻生效查询
	r, err := svc.EffectiveRule(ctx, admin, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil || r.Version != 2 {
		t.Fatalf("effective at 10-10: %+v err=%v want v2", r, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
