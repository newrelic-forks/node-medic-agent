package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// requestIDHeader carries the per-request correlation ID.
const requestIDHeader = "X-Request-ID"

// statusRecorder wraps http.ResponseWriter to capture the status code
// the handler wrote. http.ResponseWriter doesn't expose the code after
// WriteHeader, but the access logger needs it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.wrote = true
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wrote {
		sr.status = http.StatusOK
		sr.wrote = true
	}
	return sr.ResponseWriter.Write(b)
}

// Flush exposes the underlying http.Flusher when present. Drain SSE
// handlers depend on this — without it the middleware would buffer
// per-event writes until the handler returned.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withRequestID injects an X-Request-ID into every request context.
// Generates a fresh 8-byte hex ID if the inbound header is absent.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405")
	}
	return hex.EncodeToString(b[:])
}

// withAccessLog emits one structured access record per request after
// the handler returns. No payload bodies — avoids leaking LLM-authored
// evidence into logs.
func withAccessLog(logger *Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		logger.Log("http_access",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withRecover catches panics in downstream handlers, emits a
// structured log line, and returns 500. Single-replica deployment
// makes a panic-induced crash an outage; this prevents that.
func withRecover(logger *Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Log("http_panic",
					"request_id", RequestIDFromContext(r.Context()),
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withNoStoreOnActions sets Cache-Control: no-store on every POST to
// an action endpoint. Defends against a browser back-button replaying
// a mutation.
func withNoStoreOnActions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/cases/") &&
			strings.Contains(r.URL.Path, "/actions/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// requestIDCtxKey scopes the request-ID value in context.Context.
type requestIDCtxKey struct{}

// RequestIDFromContext returns the request ID attached by
// withRequestID, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return id
	}
	return ""
}
