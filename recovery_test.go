package compliance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dirCfg(t *testing.T, recoverOrphan bool) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Fsync = true
	cfg.RecoverOrphanClose = recoverOrphan
	cfg.DefaultRules = []Rule{{
		Version: 1, Name: "test", EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TaxRateBasisPoints: 0, DeductibleCategories: []string{"cost"},
	}}
	return cfg, dir
}

// 正常重启：从最近已提交位置继续，不重复应用、不跳过，结果完全一致。
func TestRestartReplaysCommittedState(t *testing.T) {
	cfg, _ := dirCfg(t, true)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")

	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := svc.Ingest(ctx, admin, Entry{
			IdempotencyKey: "e" + string(rune('a'+i)), Period: p,
			Category: "revenue", Amount: 100, OccurredAt: t0(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := svc.ClosePeriod(ctx, admin, p, t0().Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auditBefore := len(svc.aud.List())
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	view, err := svc2.GetPeriod(ctx, admin, p)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusClosed {
		t.Fatalf("status=%s want closed", view.Status)
	}
	if view.Current.Version != 1 || view.Current.Filing.Gross != 1000 ||
		len(view.Current.EntryKeys) != 10 || view.Current.ClosedAt != v1.ClosedAt {
		t.Fatalf("restored version mismatch: %+v", view.Current)
	}
	// 重启后重复幂等键仍被拒绝（没有重复应用）
	if err := svc2.Ingest(ctx, admin, Entry{IdempotencyKey: "ea", Period: p, Amount: 1, OccurredAt: t0()}); err != ErrDuplicateEntry {
		t.Fatalf("replayed entry lost dedup: %v", err)
	}
	// 新明细进入下一期，序列继续而不是从头再来
	p2 := Period("2026-10")
	if err := svc2.Ingest(ctx, admin, Entry{IdempotencyKey: "n1", Period: p2, Category: "revenue", Amount: 5, OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if seq := svc2.currentSeq(); seq <= v1.LastSeq {
		t.Fatalf("seq did not continue: %d <= %d", seq, v1.LastSeq)
	}
	// 审计日志跨重启保留
	if len(svc2.aud.List()) < auditBefore {
		t.Fatalf("audit events lost across restart: %d < %d", len(svc2.aud.List()), auditBefore)
	}
	_ = svc2.Close()
}

// 结账中途崩溃：WAL 里有 close_start 但没有 commit。
// 重启后必须与"已结账"明确区分：自动回滚为 open，可重新结账，且写审计。
func TestCrashMidCloseRecoversAndIsDistinguishable(t *testing.T) {
	cfg, _ := dirCfg(t, true)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")

	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 100, OccurredAt: t0()})
	// 直接补一条 commit 之前的意图记录后"杀死进程"（不提交、不写 abort）。
	if _, err := svc.w.append(evCloseStart, closeIntent{Period: p, RuleVersion: 1, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_ = svc.Close()

	svc2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := svc2.GetPeriod(ctx, admin, p)
	if view.Status != StatusOpen {
		t.Fatalf("orphan close must roll back to open, got %s", view.Status)
	}
	if view.Current != nil {
		t.Fatal("no committed version may appear after an aborted close")
	}
	found := false
	for _, ev := range svc2.aud.List() {
		if ev.Action == "recover_abort_close" {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery of orphan close must be audited")
	}
	// 明细没有丢失，可以重新结账
	v, err := svc2.ClosePeriod(ctx, admin, p, time.Now())
	if err != nil {
		t.Fatalf("re-close after recovery: %v", err)
	}
	if v.Filing.Gross != 100 {
		t.Fatalf("entries lost during recovery: gross=%d", v.Filing.Gross)
	}
	_ = svc2.Close()
}

// 孤儿结账的另一种策略：保留 closing 状态等待人工干预。
func TestOrphanCloseKeptForManualIntervention(t *testing.T) {
	cfg, _ := dirCfg(t, false)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")
	svc, _ := New(cfg)
	_, _ = svc.w.append(evCloseStart, closeIntent{Period: p, RuleVersion: 1, At: time.Now().UTC()})
	_ = svc.Close()

	svc2, _ := New(cfg)
	view, _ := svc2.GetPeriod(ctx, admin, p)
	if view.Status != StatusClosing || view.Current != nil {
		t.Fatalf("want closing with no committed version, got %s", view.Status)
	}
	// 人工显式放弃后回到 open
	if err := svc2.AbortClose(ctx, admin, p, "manual"); err != nil {
		t.Fatal(err)
	}
	view, _ = svc2.GetPeriod(ctx, admin, p)
	if view.Status != StatusOpen {
		t.Fatalf("after abort: %s", view.Status)
	}
	_ = svc2.Close()
}

// WAL 最后一行被写了一半（崩溃导致）：重启时截断到最后一条完整记录，不留部分写入。
func TestTruncatedTailWAL(t *testing.T) {
	cfg, dir := dirCfg(t, true)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")
	svc, _ := New(cfg)
	_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 100, OccurredAt: t0()})
	_ = svc.Close()

	// 追加一段无法解析的半截字节
	f, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":2,"kind":"entry","bod`)
	_ = f.Close()

	svc2, err := New(cfg)
	if err != nil {
		t.Fatalf("reopen with torn tail: %v", err)
	}
	view, _ := svc2.GetPeriod(ctx, admin, p)
	if view.EntryCount != 1 || view.Status != StatusOpen {
		t.Fatalf("torn tail recovery wrong: entries=%d status=%s", view.EntryCount, view.Status)
	}
	// 半截之后可以继续正常提交
	if err := svc2.Ingest(ctx, admin, Entry{IdempotencyKey: "e2", Period: p, Category: "revenue", Amount: 20, OccurredAt: t0()}); err != nil {
		t.Fatalf("ingest after torn tail: %v", err)
	}
	_ = svc2.Close()
}

// 第二次结账（重开后）中途崩溃：恢复后应回到 reopened（保留 v1），调整不丢，可再次结账。
func TestCrashDuringSecondCloseReturnsToReopened(t *testing.T) {
	cfg, _ := dirCfg(t, true)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-09")

	svc, _ := New(cfg)
	_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e1", Period: p, Category: "revenue", Amount: 1000, OccurredAt: t0()})
	if _, err := svc.ClosePeriod(ctx, admin, p, t0()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reopen(ctx, admin, p, "need adj"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AppendAdjustment(ctx, admin, Adjustment{ID: "a1", Period: p, Category: "cost", Amount: 100, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	// 模拟第二次结账在 commit 前崩溃：只留下 close_start
	ps, _ := svc.lookup(p)
	ps.lock.Lock()
	if _, err := svc.w.append(evCloseStart, closeIntent{Period: p, RuleVersion: 1, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ps.status = StatusClosing
	ps.lock.Unlock()
	_ = svc.Close()

	svc2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := svc2.GetPeriod(ctx, admin, p)
	if view.Status != StatusReopened {
		t.Fatalf("status=%s want reopened", view.Status)
	}
	if view.Current == nil || view.Current.Version != 1 || len(view.History) != 1 {
		t.Fatalf("v1 must survive: current=%+v history=%d", view.Current, len(view.History))
	}
	if view.AdjustmentCount != 1 {
		t.Fatalf("adjustment lost: %d", view.AdjustmentCount)
	}
	// 可直接再次结账，且结果与全量口径（1000 收入 - 100 抵扣）一致
	v2, err := svc2.ClosePeriod(ctx, admin, p, time.Now())
	if err != nil {
		t.Fatalf("reclose after recovery: %v", err)
	}
	if v2.Version != 2 || v2.Filing.Gross != 1000 || v2.Filing.Deductible != 100 {
		t.Fatalf("reclose wrong: %+v", v2.Filing)
	}
	_ = svc2.Close()
}
