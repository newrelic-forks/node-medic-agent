// Package drain holds the human-initiated eviction loop: the pod filter
// (DaemonSet, mirror, system-node-critical), the bounded-concurrency
// executor that streams per-pod outcomes as SSE events, and the
// process-local DrainProgress tracker that coalesces concurrent POSTs
// against the same node. Real code lands in Phase 6 (T077–T080).
package drain
