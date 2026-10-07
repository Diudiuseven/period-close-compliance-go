package compliance

import (
	"context"
	"testing"
	"time"
)

func TestPermissionDeniedAndAudited(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	reader := NewReader("bob")
	admin := NewAdmin("a")
	p := Period("2026-09")

	// 无 ingest 权限
	if err := svc.Ingest(ctx, reader, Entry{IdempotencyKey: "e1", Period: p, Amount: 1, OccurredAt: t0()}); err != ErrPermissionDenied {
		t.Fatalf("ingest: want denied got %v", err)
	}
	// 无 close 权限
	if _, err := svc.ClosePeriod(ctx, reader, p, t0()); err != ErrPermissionDenied {
		t.Fatalf("close: want denied got %v", err)
	}
	// 先正常结账，再测 reopen/rule
	if err := svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e1", Period: p, Amount: 100, Category: "revenue", OccurredAt: t0()}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, admin, p, t0()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reopen(ctx, reader, p, "x"); err != ErrPermissionDenied {
		t.Fatalf("reopen: want denied got %v", err)
	}
	if err := svc.PublishRule(ctx, reader, Rule{Version: 9, Name: "x"}); err != ErrPermissionDenied {
		t.Fatalf("rule: want denied got %v", err)
	}

	// 越权操作必须留下 allowed=false 的审计记录；操作实际未生效
	evs, err := svc.AuditEvents(ctx, admin, 0)
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	for _, ev := range evs {
		if !ev.Allowed {
			denied[ev.Action] = true
		}
	}
	for _, act := range []string{"ingest", "close", "reopen", "publish_rule"} {
		if !denied[act] {
			t.Fatalf("missing denied audit for action %s", act)
		}
	}
	// 被拒绝的规则发布没有生效
	if _, err := svc.rules.Version(9); err != ErrNotFound {
		t.Fatalf("denied rule must not be published")
	}
	// 无 audit 权限的用户不能读审计日志
	if _, err := svc.AuditEvents(ctx, reader, 0); err != ErrPermissionDenied {
		t.Fatalf("audit read: want denied got %v", err)
	}
	// 审计序号单调且 Since 可用
	all := svc.aud.List()
	if len(all) < 4 || all[0].Seq != 1 {
		t.Fatalf("audit seq not monotonic: %d events", len(all))
	}
	if got := svc.aud.Since(all[0].Seq); len(got) != len(all)-1 {
		t.Fatalf("Since filtering wrong: %d vs %d", len(got), len(all)-1)
	}
}

func TestOperatorRoleCanCloseButNotReopenOrRule(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	op := NewOperator("op")
	p := Period("2026-09")
	if err := svc.Ingest(ctx, op, Entry{IdempotencyKey: "e1", Period: p, Amount: 10, Category: "x", OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClosePeriod(ctx, op, p, time.Now()); err != nil {
		t.Fatalf("operator close: %v", err)
	}
	if err := svc.Reopen(ctx, op, p, "x"); err != ErrPermissionDenied {
		t.Fatalf("operator reopen: want denied got %v", err)
	}
}
