package drain

import (
	"sync"
	"time"
)

// State enumerates a DrainProgress's lifecycle.
type State string

const (
	StateRunning  State = "running"
	StateComplete State = "complete"
)

// Progress is the in-flight state for one drain. data-model.md §7.
//
// Counter fields are mutex-guarded so the eviction goroutine and the
// per-case page (which can read while a drain is in progress) don't
// race. State is updated under the same lock.
type Progress struct {
	mu sync.Mutex

	NodeName    string
	state       State
	StartedAt   time.Time
	TotalPods   int
	EvictedPods int
	SkippedPods int
	ErroredPods int
	CompletedAt time.Time
}

// NewProgress returns a fresh running Progress for a drain about to
// start. Total is the eligible-pod count from the pre-flight scan.
func NewProgress(nodeName string, total int, now time.Time) *Progress {
	return &Progress{
		NodeName:  nodeName,
		state:     StateRunning,
		StartedAt: now,
		TotalPods: total,
	}
}

// State returns the current lifecycle state.
func (p *Progress) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// IncEvicted increments the evicted counter.
func (p *Progress) IncEvicted() {
	p.mu.Lock()
	p.EvictedPods++
	p.mu.Unlock()
}

// IncSkipped increments the skipped counter.
func (p *Progress) IncSkipped() {
	p.mu.Lock()
	p.SkippedPods++
	p.mu.Unlock()
}

// IncErrored increments the errored counter.
func (p *Progress) IncErrored() {
	p.mu.Lock()
	p.ErroredPods++
	p.mu.Unlock()
}

// MarkComplete transitions the progress to the terminal state.
func (p *Progress) MarkComplete(now time.Time) {
	p.mu.Lock()
	p.state = StateComplete
	p.CompletedAt = now
	p.mu.Unlock()
}

// Snapshot returns a copy-of-counter view safe to marshal.
type Snapshot struct {
	NodeName    string    `json:"nodeName"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"startedAt"`
	TotalPods   int       `json:"totalPods"`
	EvictedPods int       `json:"evictedPods"`
	SkippedPods int       `json:"skippedPods"`
	ErroredPods int       `json:"erroredPods"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
}

// Snapshot returns a frozen view of the current progress, safe to
// JSON-encode.
func (p *Progress) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Snapshot{
		NodeName:    p.NodeName,
		State:       string(p.state),
		StartedAt:   p.StartedAt,
		TotalPods:   p.TotalPods,
		EvictedPods: p.EvictedPods,
		SkippedPods: p.SkippedPods,
		ErroredPods: p.ErroredPods,
		CompletedAt: p.CompletedAt,
	}
}

// Tracker coalesces concurrent drain POSTs against the same node.
//
// The handler:
//
//   - Calls TryStart(nodeName, total). If the second return value is
//     true, this call became the "owner" and got a fresh running
//     Progress. The handler runs the eviction loop and (eventually)
//     calls MarkComplete.
//   - If false, an in-flight drain already owns this node — the
//     handler responds 409 with the existing snapshot.
//
// A janitor goroutine periodically purges complete entries older
// than RetentionAfterComplete so a refresh shortly after a drain
// still sees the summary, but stale entries don't grow unbounded.
type Tracker struct {
	mu                     sync.Mutex
	entries                map[string]*Progress
	retentionAfterComplete time.Duration
	now                    func() time.Time
	stopCh                 chan struct{}
	janitorOnce            sync.Once
}

// DefaultRetentionAfterComplete is how long a Complete progress
// entry hangs around so a quick refresh sees the summary.
const DefaultRetentionAfterComplete = 5 * time.Minute

// NewTracker constructs an empty tracker. Caller must call
// (*Tracker).StartJanitor to begin the background sweep.
func NewTracker() *Tracker {
	return &Tracker{
		entries:                make(map[string]*Progress),
		retentionAfterComplete: DefaultRetentionAfterComplete,
		now:                    time.Now,
		stopCh:                 make(chan struct{}),
	}
}

// SetRetention overrides the default retention; useful in tests.
func (t *Tracker) SetRetention(d time.Duration) {
	t.mu.Lock()
	t.retentionAfterComplete = d
	t.mu.Unlock()
}

// SetNow overrides the time source; useful in tests.
func (t *Tracker) SetNow(f func() time.Time) {
	t.mu.Lock()
	t.now = f
	t.mu.Unlock()
}

// TryStart attempts to claim ownership of a drain on nodeName. Returns
// (existing, false) if another drain is in flight (state==running);
// the caller responds 409. Returns (nil, true) if no drain is in
// flight; caller calls TrackerNew to register a fresh Progress.
//
// Two-step shape so the caller can do the pre-flight pod scan
// (which gives the total) BEFORE registering, while still holding
// the start ticket — Register is what actually publishes to the
// map.
func (t *Tracker) TryStart(nodeName string) (*Progress, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.entries[nodeName]; ok && existing.State() == StateRunning {
		return existing, false
	}
	// Caller is free to proceed; the slot is theirs until Register.
	return nil, true
}

// Register publishes a fresh Progress into the map. Caller is the
// goroutine that will run the eviction loop. If a concurrent caller
// has registered first, this returns the existing entry + false.
func (t *Tracker) Register(p *Progress) (*Progress, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.entries[p.NodeName]; ok && existing.State() == StateRunning {
		return existing, false
	}
	t.entries[p.NodeName] = p
	return p, true
}

// Get returns the live Progress for a node, or nil if absent.
func (t *Tracker) Get(nodeName string) *Progress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[nodeName]
}

// Drop removes the entry for nodeName immediately (test hook; the
// production path lets the janitor clean up).
func (t *Tracker) Drop(nodeName string) {
	t.mu.Lock()
	delete(t.entries, nodeName)
	t.mu.Unlock()
}

// StartJanitor kicks off the background sweep. Idempotent — calling
// twice is safe. Stop the goroutine via Stop().
func (t *Tracker) StartJanitor(interval time.Duration) {
	t.janitorOnce.Do(func() {
		go func() {
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				select {
				case <-t.stopCh:
					return
				case <-tick.C:
					t.sweep()
				}
			}
		}()
	})
}

// Stop terminates the janitor goroutine.
func (t *Tracker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.stopCh:
		// already closed
	default:
		close(t.stopCh)
	}
}

func (t *Tracker) sweep() {
	t.mu.Lock()
	now := t.now()
	for k, p := range t.entries {
		if p.State() == StateComplete && !p.CompletedAt.IsZero() && now.Sub(p.CompletedAt) > t.retentionAfterComplete {
			delete(t.entries, k)
		}
	}
	t.mu.Unlock()
}
