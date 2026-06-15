package handlers

import (
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/server"
)

// DrainDeps bundles the inputs the drain handler needs. Phase 6
// (US4) wires the SSE eviction loop body; Phase 5 ships only this
// stub so main.go compiles and tests can probe the route.
type DrainDeps struct {
	Client           client.Client
	Namespace        string
	Logger           *server.Logger
	DrainConcurrency int
	AuditBufferSize  int
}

// Drain returns a handler for `POST /api/cases/{nhd}/actions/drain`.
// Phase 5 ships a 501 stub; Phase 6 swaps in the real SSE body.
func Drain(deps DrainDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":\"not_implemented\",\"phase\":\"5\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}
