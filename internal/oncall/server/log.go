package server

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Logger emits one JSON line per record on its writer. Spec FR-18 binds
// the structured-stdout shape for action audit lines; the same logger is
// reused for access logs, startup banners, and recovery events so a
// downstream `kubectl logs … | jq` pipeline can filter by `event`.
//
// The shape is deliberately small: ts (RFC3339 UTC), level, event, plus
// arbitrary key/value pairs the caller supplies. Higher-level helpers
// (audit.LogUIAction, etc.) wrap this to enforce per-event field sets.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	now   func() time.Time
	level string
}

// NewLogger returns a JSON logger writing to w. Pass os.Stdout in
// production. The level field is informational only — handlers don't
// gate on it today (R-1 keeps the surface tiny).
func NewLogger(w io.Writer, level string) *Logger {
	if w == nil {
		w = os.Stdout
	}
	if level == "" {
		level = "info"
	}
	return &Logger{w: w, now: time.Now, level: level}
}

// Log emits one JSON object with the supplied event name and key/value
// pairs. If the field count is odd, the dangling key is logged with an
// empty value rather than dropped.
func (l *Logger) Log(event string, fields ...any) {
	if l == nil {
		return
	}
	rec := map[string]any{
		"ts":    l.now().UTC().Format(time.RFC3339),
		"level": l.level,
		"event": event,
	}
	for i := 0; i < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			key = "field"
		}
		var val any
		if i+1 < len(fields) {
			val = fields[i+1]
		}
		rec[key] = val
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	enc := json.NewEncoder(l.w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(rec)
}

// loggerCtxKey is unexported so external packages can't accidentally
// shadow the logger.
type loggerCtxKey struct{}

// WithLogger attaches a logger to a context.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, l)
}

// LoggerFromContext returns the logger attached by WithLogger or a
// no-op logger if none is set. Never returns nil.
func LoggerFromContext(ctx context.Context) *Logger {
	if l, ok := ctx.Value(loggerCtxKey{}).(*Logger); ok && l != nil {
		return l
	}
	return NewLogger(io.Discard, "info")
}
