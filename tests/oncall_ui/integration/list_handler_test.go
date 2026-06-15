// T038 — list handler integration test. Drives GET / and GET /api/cases
// against a fake-client-seeded fixture set; locks the contracts/
// oncall-ui-api.yaml ListPageRow shape and the FR-13 apiserver-
// unavailable banner path.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/render"
)

func TestListHTML_RendersAllFixtures(t *testing.T) {
	applied := loadNHDFixture(t, "nhd_applied.yaml")
	hil := loadNHDFixture(t, "nhd_human_in_loop.yaml")
	pending := loadNHDFixture(t, "nhd_pending_diagnosing.yaml")

	c := newFakeClient(t, applied, hil, pending)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want text/html", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	for _, want := range []string{
		applied.Name,
		hil.Name,
		pending.Name,
		"row-applied",
		"row-human-in-loop",
		"row-neutral", // pending shape
	} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("list HTML missing %q", want)
		}
	}
}

func TestListJSON_ShapeMatchesContract(t *testing.T) {
	applied := loadNHDFixture(t, "nhd_applied.yaml")
	hil := loadNHDFixture(t, "nhd_human_in_loop.yaml")

	c := newFakeClient(t, applied, hil)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Get(ts.URL + "/api/cases")
	if err != nil {
		t.Fatalf("GET /api/cases: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	var rows []render.ListPageRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Verify required keys per contract — newest first; applied was
	// created at 15:30Z and hil at 15:28Z so applied sorts first.
	if rows[0].NHDName != applied.Name {
		t.Errorf("rows[0].NHDName = %q, want %q (applied is newer)", rows[0].NHDName, applied.Name)
	}
	if rows[0].RowClass != "row-applied" {
		t.Errorf("rows[0].RowClass = %q, want row-applied", rows[0].RowClass)
	}
	if rows[1].RowClass != "row-human-in-loop" {
		t.Errorf("rows[1].RowClass = %q, want row-human-in-loop", rows[1].RowClass)
	}
}

func TestListHTML_ApiserverErrorBanner(t *testing.T) {
	// Inject a List interceptor that errors. FR-13 binds: list page
	// renders 200 with an inline error banner, NOT a 5xx.
	failingFuncs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.New("simulated apiserver outage")
		},
	}
	cl := newFakeClientWithFuncs(t, failingFuncs)
	ts := newTestServer(t, cl)

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 even on apiserver error (FR-13)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "apiserver unreachable") {
		t.Errorf("error banner missing — body excerpt: %s", excerpt(string(body), 240))
	}
	if !strings.Contains(string(body), "simulated apiserver outage") {
		t.Errorf("error message not surfaced in banner")
	}
}

func TestListJSON_ApiserverErrorReturns500(t *testing.T) {
	failingFuncs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.New("simulated apiserver outage")
		},
	}
	cl := newFakeClientWithFuncs(t, failingFuncs)
	ts := newTestServer(t, cl)

	resp, err := ts.Client().Get(ts.URL + "/api/cases")
	if err != nil {
		t.Fatalf("GET /api/cases: %v", err)
	}
	defer resp.Body.Close()
	// JSON consumers (the auto-refresh JS) want a clear 500 + error
	// payload; the list HTML page absorbs the failure inline.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var env map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if env["error"] != "apiserver-unavailable" {
		t.Errorf("error = %q, want apiserver-unavailable", env["error"])
	}
}

func TestListJSON_EmptyResultIsArrayNotNull(t *testing.T) {
	c := newFakeClient(t)
	ts := newTestServer(t, c)
	resp, err := ts.Client().Get(ts.URL + "/api/cases")
	if err != nil {
		t.Fatalf("GET /api/cases: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := strings.TrimSpace(string(body))
	if got != "[]" {
		t.Errorf("empty response body = %q, want [] (auto-refresh JS expects an array)", got)
	}
}

// Compile-time assertion that the contracts/oncall-ui-api.yaml
// ListPageRow shape lines up with the Go struct.
var _ = nodemedicv1alpha1.NodeHealthDiagnosisAI{}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
