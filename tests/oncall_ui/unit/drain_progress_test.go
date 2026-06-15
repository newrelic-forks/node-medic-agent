// T075 — Tracker / Progress lifecycle tests (data-model.md §7).
//
// Run with -race to catch counter races; the in-flight progress
// is a concurrent surface (eviction goroutines + handler-side reads).
package unit

import (
	"sync"
	"testing"
	"time"

	"k8s.io/node-problem-detector/internal/oncall/drain"
)

func TestTracker_TryStart_NoExisting(t *testing.T) {
	tr := drain.NewTracker()
	defer tr.Stop()
	existing, ok := tr.TryStart("node-a")
	if !ok {
		t.Errorf("TryStart on empty tracker = (_, false), want (_, true)")
	}
	if existing != nil {
		t.Errorf("TryStart returned existing = %v, want nil", existing)
	}
}

func TestTracker_Register_AndConflict(t *testing.T) {
	tr := drain.NewTracker()
	defer tr.Stop()
	now := time.Now()

	p := drain.NewProgress("node-a", 5, now)
	got, ok := tr.Register(p)
	if !ok {
		t.Errorf("first Register = (_, false), want true")
	}
	if got != p {
		t.Errorf("Register returned different pointer than passed in")
	}

	// Second Register on the same running entry returns existing + false.
	p2 := drain.NewProgress("node-a", 7, now)
	got2, ok2 := tr.Register(p2)
	if ok2 {
		t.Errorf("Register on running entry = (_, true), want false (conflict)")
	}
	if got2 != p {
		t.Errorf("conflict Register returned %v, want existing %v", got2, p)
	}
}

func TestTracker_TryStart_AfterComplete(t *testing.T) {
	tr := drain.NewTracker()
	defer tr.Stop()
	now := time.Now()

	p := drain.NewProgress("node-a", 5, now)
	tr.Register(p)
	p.MarkComplete(now.Add(2 * time.Second))

	// After Complete, TryStart succeeds (a new drain is allowed).
	existing, ok := tr.TryStart("node-a")
	if !ok {
		t.Errorf("TryStart after Complete = (_, false), want true")
	}
	if existing != nil {
		t.Errorf("TryStart after Complete returned existing = %v, want nil (state was complete)", existing)
	}
}

func TestProgress_CounterRaceFreedom(t *testing.T) {
	p := drain.NewProgress("node-a", 100, time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(3)
		go func() { p.IncEvicted(); wg.Done() }()
		go func() { p.IncSkipped(); wg.Done() }()
		go func() { p.IncErrored(); wg.Done() }()
	}
	wg.Wait()
	snap := p.Snapshot()
	if snap.EvictedPods != 30 || snap.SkippedPods != 30 || snap.ErroredPods != 30 {
		t.Errorf("counters = %d/%d/%d, want 30/30/30", snap.EvictedPods, snap.SkippedPods, snap.ErroredPods)
	}
}

func TestTracker_JanitorDropsExpired(t *testing.T) {
	tr := drain.NewTracker()
	tr.SetRetention(50 * time.Millisecond)
	defer tr.Stop()
	now := time.Now()
	p := drain.NewProgress("node-a", 1, now)
	tr.Register(p)
	p.MarkComplete(now)

	// Drive sweeping by hand — start the janitor at a short interval
	// and wait long enough for the entry to expire.
	tr.StartJanitor(20 * time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if got := tr.Get("node-a"); got != nil {
		t.Errorf("janitor did not drop expired entry; Get = %+v", got.Snapshot())
	}
}

func TestTracker_DoubleStop(t *testing.T) {
	tr := drain.NewTracker()
	tr.Stop()
	tr.Stop() // must not panic
}
