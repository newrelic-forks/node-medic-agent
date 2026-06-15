// T058 — captures stdout via a custom io.Writer injected into the
// shared logger; verifies every action handler emits exactly one
// `event=ui_action` JSON line per FR-18, even on idempotent / 400 /
// 500 paths.
package integration

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/handlers"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// thread-safe writer for the test logger.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newCapturingTestServer(t *testing.T, c client.Client) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	buf := &lockedBuffer{}
	logger := server.NewLogger(buf, "info")

	srv := server.NewStubServer()
	// Replace the server's internal logger by re-wiring via a custom
	// constructor. NewStubServer doesn't accept a logger, so we rely
	// on reflection-free indirection: build deps with the capturing
	// logger directly.
	listDeps := handlers.ListDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: logger}
	detailDeps := handlers.DetailDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: logger}
	srv.SetRouteHandler("GET /", handlers.ListHTML(listDeps))
	srv.SetRouteHandler("GET /api/cases", handlers.ListJSON(listDeps))
	srv.SetRouteHandler("GET /cases/{nhd}", handlers.DetailHTML(detailDeps))

	actDeps := handlers.ActionDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: logger, AuditBufferSize: srv.Cfg().AuditBufferSize}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/uncordon", handlers.Uncordon(actDeps))
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/clear-skip-deletion", handlers.ClearSkipDeletion(actDeps))

	drainDeps := handlers.DrainDeps{Client: c, Namespace: srv.Cfg().Namespace, Logger: logger, DrainConcurrency: srv.Cfg().DrainConcurrency, AuditBufferSize: srv.Cfg().AuditBufferSize}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/drain", handlers.Drain(drainDeps))

	srv.SetStaticHandler(staticHandler())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, buf
}

func TestActionAuditLog_LineShape(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true, true)
	c := newFakeClient(t, nhd, node)
	ts, buf := newCapturingTestServer(t, c)

	// Drive one uncordon + one clear-skip-deletion.
	if resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil); resp != nil {
		resp.Body.Close()
	}
	if resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/clear-skip-deletion", "application/json", nil); resp != nil {
		resp.Body.Close()
	}

	lines := uiActionLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("ui_action lines = %d, want 2", len(lines))
	}

	requiredKeys := []string{"ts", "event", "action", "nhd_name", "node", "actor", "result", "duration_ms"}
	for i, line := range lines {
		if line["event"] != "ui_action" {
			t.Errorf("line[%d].event = %q, want ui_action", i, line["event"])
		}
		for _, k := range requiredKeys {
			if _, ok := line[k]; !ok {
				t.Errorf("line[%d] missing key %q", i, k)
			}
		}
		if line["actor"] != "demo-anonymous" {
			t.Errorf("line[%d].actor = %q, want demo-anonymous", i, line["actor"])
		}
	}

	// Order: uncordon first, then clear-skip-deletion.
	if lines[0]["action"] != "uncordon" {
		t.Errorf("line[0].action = %q, want uncordon", lines[0]["action"])
	}
	if lines[1]["action"] != "clear-skip-deletion" {
		t.Errorf("line[1].action = %q, want clear-skip-deletion", lines[1]["action"])
	}
}

func TestActionAuditLog_NoChangePathStillLogs(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	// Already-uncordoned + no-skip-deletion node so both endpoints
	// hit their no-change branches.
	node := nodeFor(nhd.Spec.Case.NodeName, false, false)
	c := newFakeClient(t, nhd, node)
	ts, buf := newCapturingTestServer(t, c)

	if resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil); resp != nil {
		resp.Body.Close()
	}
	if resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/clear-skip-deletion", "application/json", nil); resp != nil {
		resp.Body.Close()
	}

	lines := uiActionLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("FR-18: every attempt logs even no-change paths; got %d lines", len(lines))
	}
	for i, line := range lines {
		if line["result"] != "no-change" {
			t.Errorf("line[%d].result = %q, want no-change", i, line["result"])
		}
	}
}

// uiActionLines parses each NDJSON record on stdout and returns the
// ones whose `event` is ui_action. duration_ms decodes as float so
// callers compare via Sprint.
func uiActionLines(blob string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, line := range strings.Split(strings.TrimSpace(blob), "\n") {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "ui_action" {
			out = append(out, rec)
		}
	}
	return out
}
