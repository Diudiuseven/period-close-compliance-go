package periodclose

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentPeriodsDoNotBlock(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

	const n = 12
	var wg sync.WaitGroup
	// Each period ingests concurrently, then closes concurrently.
	for i := 0; i < n; i++ {
		p := Period(fmt.Sprintf("2026%02d", i+1))
		wg.Add(1)
		go func(p Period) {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				if err := s.SubmitEntries(submitter, Entry{
					ID: fmt.Sprintf("%s-%d", p, k), Period: p,
					Category: "sales", Amount: int64(k + 1),
				}); err != nil {
					t.Errorf("submit %s: %v", p, err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		p := Period(fmt.Sprintf("2026%02d", i+1))
		wg.Add(1)
		go func(p Period) {
			defer wg.Done()
			cv, err := s.ClosePeriod(closer, p)
			if err != nil {
				t.Errorf("close %s: %v", p, err)
				return
			}
			if cv.Report.Net != 20*21/2 { // sum 1..20 = 210
				t.Errorf("period %s net=%d want 210", p, cv.Report.Net)
			}
		}(p)
	}
	wg.Wait()
}

// Concurrent closes of the SAME period must produce exactly one committed
// version; losers get ErrPeriodClosed/ErrClosing, and the stored report must
// never be a half-written value.
func TestConcurrentSamePeriodSingleCommit(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})
	if err := s.SubmitEntries(submitter,
		Entry{ID: "e1", Period: "202601", Category: "sales", Amount: 42}); err != nil {
		t.Fatal(err)
	}
	const g = 20
	var wg sync.WaitGroup
	var commits, rejects int32
	var mu sync.Mutex
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cv, err := s.ClosePeriod(closer, "202601")
			mu.Lock()
			defer mu.Unlock()
			if err == nil && cv != nil {
				commits++
			} else {
				rejects++
			}
		}()
	}
	wg.Wait()
	if commits != 1 || commits+rejects != g {
		t.Fatalf("commits=%d rejects=%d", commits, rejects)
	}
	pp := s.periods["202601"]
	if pp.Status != StatusClosed || pp.CloseSeq != 1 ||
		pp.Current.Report.Net != 42 {
		t.Fatalf("unexpected final state: %+v", pp)
	}
}

// A reader running concurrently with closes/intake must always observe a
// self-consistent view (no nil-but-closed / mixed fields).
func TestConcurrentReadersSeeConsistentViews(t *testing.T) {
	s, _ := testSvc(t)
	defer s.Close()
	mustRule(t, s, Rule{})

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
				v, err := s.GetPeriod(reader, "202601")
				if err == nil && v.Status == StatusClosed {
					if v.CloseVersion == nil || v.CloseVersion.Version == 0 {
						t.Errorf("closed but no version: %+v", v)
						return
					}
					if v.CloseVersion.Report.Net != 55 {
						t.Errorf("torn read net=%d", v.CloseVersion.Report.Net)
						return
					}
				}
			}
		}
	}()
	for k := 0; k < 10; k++ {
		if err := s.SubmitEntries(submitter, Entry{ID: string(rune('a'+k)) + "x",
			Period: "202601", Category: "sales", Amount: int64(k + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClosePeriod(closer, "202601"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}
