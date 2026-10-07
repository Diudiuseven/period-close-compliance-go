package compliance

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 慢期间的结账不得阻塞其他期间的明细摄入或结账推进。
func TestSlowCloseDoesNotBlockOtherPeriods(t *testing.T) {
	svc := testSvc(t)
	svc.cfg.CloseTimeout = 0 // 允许等待
	ctx := context.Background()
	admin := NewAdmin("a")
	slow := Period("2026-01")
	fast := Period("2026-02")

	// 人为占住 slow 期间的期间锁模拟"慢结账"
	slowPS := svc.getOrCreate(slow)
	slowPS.lock.Lock()

	done := make(chan struct{})
	go func() {
		// 这会阻塞直到我们释放 slow 锁
		_, _ = svc.ClosePeriod(ctx, admin, slow, time.Now())
		close(done)
	}()

	// 在 slow 阻塞期间，fast 期间的摄入与结账必须照常完成
	for i := 0; i < 50; i++ {
		e := Entry{IdempotencyKey: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Period: fast, Category: "revenue", Amount: 100, OccurredAt: time.Now()}
		if err := svc.Ingest(ctx, admin, e); err != nil {
			t.Fatalf("fast ingest %d: %v", i, err)
		}
	}
	fv, err := svc.ClosePeriod(ctx, admin, fast, time.Now())
	if err != nil {
		t.Fatalf("fast close while slow blocked: %v", err)
	}
	if fv.Filing.Gross != 5000 {
		t.Fatalf("fast gross = %d want 5000", fv.Filing.Gross)
	}

	select {
	case <-done:
		t.Fatal("slow close must still be blocked")
	default:
	}
	slowPS.lock.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("slow close did not finish after unlock")
	}
}

// 并发对同一期间结账：恰好一个成功，期间最终只有唯一已提交版本。
func TestConcurrentCloseOneWinner(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-03")
	for i := 0; i < 20; i++ {
		_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: "e" + string(rune('a'+i)), Period: p, Category: "revenue", Amount: 100, OccurredAt: time.Now()})
	}
	var wg sync.WaitGroup
	ok, closed := 0, 0
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.ClosePeriod(ctx, admin, p, time.Now())
			mu.Lock()
			defer mu.Unlock()
			switch err {
			case nil:
				ok++
			case ErrPeriodClosed, ErrPeriodClosing:
				closed++
			default:
				t.Errorf("unexpected close err: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || ok+closed != 10 {
		t.Fatalf("ok=%d closed=%d", ok, closed)
	}
	view, _ := svc.GetPeriod(ctx, admin, p)
	if view.Status != StatusClosed || view.Current.Version != 1 || view.Current.Filing.Gross != 2000 {
		t.Fatalf("bad final view: status=%s", view.Status)
	}
}

// 并发摄入相同/不同明细：每条键恰好计入一次。
func TestConcurrentIngestDedup(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	admin := NewAdmin("a")
	p := Period("2026-04")
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('0'+i%50)) // 50 个键，各被抢 4 次
			_ = svc.Ingest(ctx, admin, Entry{IdempotencyKey: key, Period: p, Category: "revenue", Amount: 100, OccurredAt: time.Now()})
		}(i)
	}
	wg.Wait()
	v, err := svc.ClosePeriod(ctx, admin, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.EntryKeys) != 50 || v.Filing.Gross != 5000 {
		t.Fatalf("entries=%d gross=%d want 50/5000", len(v.EntryKeys), v.Filing.Gross)
	}
}
