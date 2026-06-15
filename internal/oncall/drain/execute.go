package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodEvent is the per-pod SSE payload. JSON tags lock the
// contract shape (contracts/oncall-ui-api.yaml DrainPodEvent).
type PodEvent struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Result    string `json:"result"` // "evicted" | "skipped" | "error"
	Detail    string `json:"detail,omitempty"`
}

// CompleteEvent is the terminal SSE payload (event: complete).
type CompleteEvent struct {
	Evicted    int   `json:"evicted"`
	Skipped    int   `json:"skipped"`
	Errored    int   `json:"errored"`
	DurationMs int64 `json:"durationMs"`
}

// SSEFlusher is what RunEviction needs from its writer to push events
// without buffering. http.ResponseWriter satisfies this when the
// underlying server supports flushing.
type SSEFlusher interface {
	io.Writer
	http.Flusher
}

// WriteSSEHeaders sets the standard event-stream response headers
// per data-model.md §8 + research R-7. Caller writes them BEFORE
// the first event.
func WriteSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// writePodFrame emits a default-event SSE frame with the given JSON
// payload.
func writePodFrame(w SSEFlusher, ev PodEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", string(b)); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// writeCompleteFrame emits the terminal `event: complete` frame.
func writeCompleteFrame(w SSEFlusher, ev CompleteEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: complete\ndata: %s\n\n", string(b)); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// EvictPodsConfig bundles the inputs RunEviction needs.
type EvictPodsConfig struct {
	Concurrency int
	GracePeriod *int64 // nil → apiserver default
}

// RunEviction issues evictions for each eligible pod under the
// configured concurrency cap, streams per-pod outcomes to w, and
// returns the terminal counters. Skipped pods are surfaced as a
// `result=skipped` SSE event so the consumer sees motion for every
// candidate. Errors (PDB violations etc.) are surfaced as
// `result=error` and DO NOT abort the loop (FR-16).
//
// The eviction loop uses a buffered channel as a semaphore so
// in-flight evictions stay bounded at cfg.Concurrency. Per-pod
// SSE writes are funneled through a write-mutex because multiple
// goroutines complete in arrival order — the writer is shared.
func RunEviction(ctx context.Context, c client.Client, w SSEFlusher, decisions []PodDecision, progress *Progress, cfg EvictPodsConfig) CompleteEvent {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 3
	}
	started := time.Now()

	// Skipped pods first — emit synchronously and bump the counter.
	// The user sees DaemonSet/mirror/system-critical pods filtered
	// before the eviction loop kicks in.
	var eligible []PodDecision
	var writeMu sync.Mutex
	for _, d := range decisions {
		if !d.Eligible {
			ev := PodEvent{
				Pod:       d.Pod.Name,
				Namespace: d.Pod.Namespace,
				Result:    "skipped",
				Detail:    string(d.Reason),
			}
			writeMu.Lock()
			_ = writePodFrame(w, ev)
			writeMu.Unlock()
			progress.IncSkipped()
			continue
		}
		eligible = append(eligible, d)
	}

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	for _, d := range eligible {
		select {
		case <-ctx.Done():
			break
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pod *corev1.Pod) {
			defer wg.Done()
			defer func() { <-sem }()
			result, detail := evictOne(ctx, c, pod, cfg.GracePeriod)
			ev := PodEvent{
				Pod:       pod.Name,
				Namespace: pod.Namespace,
				Result:    result,
				Detail:    detail,
			}
			writeMu.Lock()
			_ = writePodFrame(w, ev)
			writeMu.Unlock()
			switch result {
			case "evicted":
				progress.IncEvicted()
			case "error":
				progress.IncErrored()
			}
		}(d.Pod)
	}
	wg.Wait()

	summary := CompleteEvent{
		Evicted:    progress.Snapshot().EvictedPods,
		Skipped:    progress.Snapshot().SkippedPods,
		Errored:    progress.Snapshot().ErroredPods,
		DurationMs: time.Since(started).Milliseconds(),
	}
	writeMu.Lock()
	_ = writeCompleteFrame(w, summary)
	writeMu.Unlock()
	return summary
}

// evictOne runs the apiserver-side `pods/eviction` subresource
// create. Returns the result string + a detail string describing
// any error.
func evictOne(ctx context.Context, c client.Client, pod *corev1.Pod, gracePeriod *int64) (string, string) {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{},
	}
	if gracePeriod != nil {
		eviction.DeleteOptions.GracePeriodSeconds = gracePeriod
	}
	// SubResource("eviction").Create requires the parent pod for the
	// apiserver to derive the URL; the eviction's metadata.name +
	// namespace must match.
	if err := c.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		// 429 Too Many Requests = PDB violation.
		if apierrors.IsTooManyRequests(err) {
			return "error", trimErr(err.Error())
		}
		// Forbidden / NotFound on the pod (e.g. the pod went away
		// between pre-flight and eviction) — surface as error.
		return "error", trimErr(err.Error())
	}
	return "evicted", ""
}

// trimErr keeps SSE payloads under audit-history.detail's 200-byte
// cap (data-model.md §3) so a downstream audit-summary write doesn't
// have to retruncate.
func trimErr(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
