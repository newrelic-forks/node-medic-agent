package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
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

// Server is the on-call UI's HTTP service. The route table is built
// once in New(); each product route reads its handler from the
// `routes` map at request time. SetRouteHandler replaces a stubbed
// 501 handler with a real one (or vice versa) without re-routing
// the mux. This keeps the route table immutable from the test
// harness's perspective while letting Phases 4/5/6 swap real
// handlers in.
type Server struct {
	cfg      Config
	kc       client.Client
	logger   *Logger
	mux      *http.ServeMux
	routes   map[string]http.HandlerFunc
	routesMu sync.RWMutex
	staticH  http.Handler
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
		routes: make(map[string]http.HandlerFunc),
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
		routes: make(map[string]http.HandlerFunc),
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

// registerRoutes wires the six product routes plus /healthz and the
// /static/ asset prefix. Each product-route key indexes into
// s.routes; SetRouteHandler swaps the live handler in place. Until a
// later phase calls SetRouteHandler the route serves its stub.
//
// The static asset handler is set lazily by SetStaticHandler — Phase
// 2 carries no static assets, so it returns 404 until rendered
// templates land in Phase 4.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	stubHTML := s.handleStubHTML
	stubJSON := s.handleStubJSON
	stubSSE := s.handleStubSSE

	s.routes["GET /"] = stubHTML
	s.routes["GET /api/cases"] = stubJSON
	s.routes["GET /cases/{nhd}"] = stubHTML
	s.routes["POST /api/cases/{nhd}/actions/uncordon"] = stubJSON
	s.routes["POST /api/cases/{nhd}/actions/clear-skip-deletion"] = stubJSON
	s.routes["POST /api/cases/{nhd}/actions/drain"] = stubSSE

	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// ServeMux's GET pattern matches the root; subroutes for
		// /cases/, /api/, /static/ are registered explicitly below.
		// ServeMux still routes to the most specific match.
		s.dispatch("GET /", w, r)
	})
	s.mux.HandleFunc("GET /api/cases", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch("GET /api/cases", w, r)
	})
	s.mux.HandleFunc("GET /cases/{nhd}", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch("GET /cases/{nhd}", w, r)
	})
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/uncordon", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch("POST /api/cases/{nhd}/actions/uncordon", w, r)
	})
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/clear-skip-deletion", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch("POST /api/cases/{nhd}/actions/clear-skip-deletion", w, r)
	})
	s.mux.HandleFunc("POST /api/cases/{nhd}/actions/drain", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch("POST /api/cases/{nhd}/actions/drain", w, r)
	})

	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		s.routesMu.RLock()
		h := s.staticH
		s.routesMu.RUnlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// SetRouteHandler swaps a route's handler in place. Caller passes the
// exact key registered in registerRoutes.
func (s *Server) SetRouteHandler(key string, h http.HandlerFunc) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	if _, ok := s.routes[key]; !ok {
		// Mis-typed key — surface in tests instead of silently
		// becoming a 501.
		panic("server: unknown route key: " + key)
	}
	s.routes[key] = h
}

// SetStaticHandler attaches the /static/ asset handler. Phase 4
// passes an http.FileServer wrapping render.StaticFS().
func (s *Server) SetStaticHandler(h http.Handler) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	s.staticH = h
}

// Client returns the kube client wired into the server. Handlers
// constructed against this server use it for apiserver round-trips.
func (s *Server) Client() client.Client { return s.kc }

// Config returns a copy of the server's effective configuration.
// Handlers read Namespace, UIBaseURL, etc. from this.
func (s *Server) Cfg() Config { return s.cfg }

// dispatch routes a request to the current handler under key.
func (s *Server) dispatch(key string, w http.ResponseWriter, r *http.Request) {
	s.routesMu.RLock()
	h := s.routes[key]
	s.routesMu.RUnlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
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
