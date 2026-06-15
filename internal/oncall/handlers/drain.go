package handlers

import (
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/audit"
	"k8s.io/node-problem-detector/internal/oncall/drain"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// DrainDeps bundles the inputs the drain handler needs.
type DrainDeps struct {
	Client           client.Client
	Namespace        string
	Logger           *server.Logger
	DrainConcurrency int
	AuditBufferSize  int
	Now              func() time.Time
	// Tracker is the process-local in-flight drain tracker. If nil,
	// Drain creates one lazily on first use; tests can pre-construct
	// one to share across multiple Drain handler instances.
	Tracker *drain.Tracker
}

// Drain is FR-16: list pods on nhd.spec.case.nodeName (excluding
// DaemonSet, mirror, system-node-critical, terminating pods), evict
// each via pods/eviction, stream per-pod outcomes as SSE, and on
// completion write a single audit entry summarizing the drain.
//
// Concurrent second POST against the same node returns 409 with the
// in-flight progress (FR-9 G9).
func Drain(deps DrainDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	tracker := deps.Tracker
	if tracker == nil {
		tracker = drain.NewTracker()
		tracker.StartJanitor(30 * time.Second)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		nhdName := r.PathValue("nhd")
		ctx := r.Context()
		started := now()

		nhd, err := fetchNHD(ctx, deps.Client, deps.Namespace, nhdName)
		if err != nil {
			handleNHDFetchErr(w, r, deps.Logger, started, "drain", nhdName, "", err)
			return
		}

		nodeName := nhd.Spec.Case.NodeName
		node, err := fetchNode(ctx, deps.Client, nodeName)

		// FR-17a — reclaimed branch: 200 + empty SSE stream + complete.
		if err != nil && nodeReclaimed(err) {
			result := "node no longer exists (already reclaimed)"
			drain.WriteSSEHeaders(w)
			w.WriteHeader(http.StatusOK)
			f, ok := w.(drain.SSEFlusher)
			if !ok {
				deps.Logger.Log("sse_no_flusher", "request_id", server.RequestIDFromContext(ctx))
				return
			}
			summary := drain.CompleteEvent{Evicted: 0, Skipped: 0, Errored: 0, DurationMs: time.Since(started).Milliseconds()}
			_ = writeCompleteFrameOnly(f, summary)

			if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
				Ts:     now(),
				Action: "drain",
				Actor:  ActorDemoAnonymous,
				Result: "reclaimed",
				Detail: result,
			}); patchErr != nil {
				deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "drain", "error", patchErr.Error())
			}
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, "reclaimed", time.Since(started).Milliseconds())
			return
		}
		if err != nil {
			handleNodeFetchErr(w, r, deps.Logger, started, "drain", nhdName, nodeName, err)
			return
		}

		if validateErr := validateNodeMatchesCase(nhd, node); validateErr != nil {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "node-mismatch",
				"message": validateErr.Error(),
			})
			return
		}

		// In-flight coalescing — second click during a running drain
		// returns 409 with progress.
		if existing, ok := tracker.TryStart(nodeName); !ok {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "drain-in-progress",
				"message":  "a drain is already in flight on this node",
				"progress": existing.Snapshot(),
			})
			return
		}

		// Pre-flight pod scan.
		decisions, planErr := drain.PlanForNode(ctx, deps.Client, nodeName)
		if planErr != nil {
			deps.Logger.Log("apiserver_list_pods_failed", "request_id", server.RequestIDFromContext(ctx), "node", nodeName, "error", planErr.Error())
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":   "apiserver-unavailable",
				"message": planErr.Error(),
			})
			return
		}
		eligibleCount := 0
		for _, d := range decisions {
			if d.Eligible {
				eligibleCount++
			}
		}
		progress := drain.NewProgress(nodeName, eligibleCount, now())
		if _, ok := tracker.Register(progress); !ok {
			// Race-loser path: another goroutine registered between
			// TryStart and Register. Re-emit a 409 with the
			// existing snapshot.
			existing := tracker.Get(nodeName)
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			snapshot := drain.Snapshot{}
			if existing != nil {
				snapshot = existing.Snapshot()
			}
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "drain-in-progress",
				"message":  "a drain is already in flight on this node",
				"progress": snapshot,
			})
			return
		}

		// Send SSE response.
		drain.WriteSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(drain.SSEFlusher)
		if !ok {
			deps.Logger.Log("sse_no_flusher", "request_id", server.RequestIDFromContext(ctx))
			progress.MarkComplete(now())
			return
		}

		summary := drain.RunEviction(ctx, deps.Client, flusher, decisions, progress, drain.EvictPodsConfig{
			Concurrency: deps.DrainConcurrency,
		})

		progress.MarkComplete(now())

		// Audit annotation: one entry summarizing the drain. Result
		// is "ok" if no errors, "partial" otherwise.
		auditResult := "ok"
		if summary.Errored > 0 || summary.Skipped > 0 {
			auditResult = "partial"
		}
		detail := fmt.Sprintf("evicted=%d skipped=%d errored=%d", summary.Evicted, summary.Skipped, summary.Errored)
		if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
			Ts:     now(),
			Action: "drain",
			Actor:  ActorDemoAnonymous,
			Result: auditResult,
			Detail: detail,
		}); patchErr != nil {
			deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "drain", "error", patchErr.Error())
		}

		audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "drain", nhdName, nodeName, ActorDemoAnonymous, auditResult, time.Since(started).Milliseconds())
	}
}

// writeCompleteFrameOnly emits just the terminal `event: complete`
// frame. Used by the reclaimed-node branch which has no per-pod
// events to send.
func writeCompleteFrameOnly(w drain.SSEFlusher, ev drain.CompleteEvent) error {
	// Marshal the event payload via the drain package's helpers by
	// piggy-backing on the event type. We can't reach the unexported
	// writeCompleteFrame; replicate the wire shape here so we don't
	// need to widen the drain package's surface.
	return writeBareCompleteFrame(w, ev)
}

func writeBareCompleteFrame(w drain.SSEFlusher, ev drain.CompleteEvent) error {
	// Minimal shape: event line + data line + blank line.
	body := fmt.Sprintf(`{"evicted":%d,"skipped":%d,"errored":%d,"durationMs":%d}`,
		ev.Evicted, ev.Skipped, ev.Errored, ev.DurationMs)
	if _, err := fmt.Fprintf(w, "event: complete\ndata: %s\n\n", body); err != nil {
		return err
	}
	w.Flush()
	return nil
}
