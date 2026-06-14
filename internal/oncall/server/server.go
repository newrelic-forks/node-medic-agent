package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config bundles every flag/env-driven knob the server reads. Mirrors
// data-model.md §9.
type Config struct {
	ListenAddr       string
	ClusterName      string
	Namespace        string
	UIBaseURL        string
	LogLevel         string
	DrainConcurrency int
	AuditBufferSize  int
}

// Server is the on-call UI's HTTP service. Phase 2 ships the route
// table with stub handlers (501 Not Implemented for every endpoint);
// later phases attach real handlers via SetHandler.
type Server struct {
	cfg    Config
	kc     client.Client
	logger *Logger
	mux    *http.ServeMux
}

// New builds a Server with the given config and kube client. The kube
// client may be nil for stub-only test wiring; production callers
// must pass a non-nil client.
func New(cfg Config, kc client.Client) (*Server, error) {
	if cfg.ClusterName == "" {
		return nil, errors.New("server: cfg.ClusterName must be set (caller MUST validateClusterName first)")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "cf-monitoring"
	}
	if cfg.DrainConcurrency <= 0 {
		cfg.DrainConcurrency = 3
	}
	if cfg.AuditBufferSize <= 0 {
		cfg.AuditBufferSize = 20
	}
	logger := NewLogger(os.Stdout, cfg.LogLevel)
	s := &Server{
		cfg:    cfg,
		kc:     kc,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

// NewStubServer returns a Server with a nil kube client and stdout
// silenced. Intended for unit/integration tests that exercise the
// route table without wiring real apiserver state.
func NewStubServer() *Server {
	logger := NewLogger(stubDiscard{}, "info")
	s := &Server{
		cfg: Config{
			ListenAddr:       ":0",
			ClusterName:      "test-stub",
			Namespace:        "cf-monitoring",
			UIBaseURL:        "http://localhost:8080",
			DrainConcurrency: 3,
			AuditBufferSize:  20,
		},
		kc:     nil,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// stubDiscard silences logger output during tests.
type stubDiscard struct{}

func (stubDiscard) Write(p []byte) (int, error) { return len(p), nil }

// Handler returns the assembled http.Handler: route table wrapped in
// the middleware chain. Tests reach for this; production callers use
// Run.
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = withNoStoreOnActions(h)
	h = withAccessLog(s.logger, h)
	h = withRecover(s.logger, h)
	h = withRequestID(h)
	return h
}

// Logger returns the server's structured logger so handlers in this
// package + cmd/main can emit on the same pipe.
func (s *Server) Logger() *Logger { return s.logger }

// Run starts the HTTP server and blocks until ctx is cancelled. On
// shutdown it gives in-flight requests up to 10s to drain (drain SSE
// streams may run longer; their goroutines complete server-side
// regardless of whether the client is attached).
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logger.Log("server_listen", "addr", s.cfg.ListenAddr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Log("server_shutdown", "reason", ctx.Err().Error())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err, ok := <-errCh:
		if ok && err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	}
}

// registerRoutes wires the six routes per plan + contracts/oncall-ui-api.yaml,
// plus a /healthz probe target. Real handler bodies for the six product
// routes arrive in Phases 4/5/6; Phase 2 stubs return 501 with the
// Content-Type each consumer expects. /healthz returns 200 from Phase 2
// onward so the kubelet probes don't require the product handlers to be
// real before the pod is marked Ready.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /", s.handleStubHTML)
	s.mux.HandleFunc("GET /api/cases", s.handleStubJSON)
	s.mux.HandleFunc("GET /cases/{nhd}", s.handleStubHTML)
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/uncordon", s.handleStubJSON)
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/clear-skip-deletion", s.handleStubJSON)
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/drain", s.handleStubSSE)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleStubHTML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte("<!doctype html><title>Not Implemented</title><p>Phase 2 stub.</p>"))
}

func (s *Server) handleStubJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "not_implemented",
		"phase": "2",
	})
}

func (s *Server) handleStubSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	// no-store satisfies the action-endpoint invariant (browser back-button
	// MUST NOT replay a mutation); no-cache is the SSE-spec recommendation
	// (research R-7). Combining both is valid HTTP and forward-compatible
	// with any proxy in front.
	w.Header().Set("Cache-Control", "no-store, no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte("event: error\ndata: {\"error\":\"not_implemented\",\"phase\":\"2\"}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
