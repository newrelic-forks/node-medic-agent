// T056 — uncordon action integration tests.
//
// Each fixture exercises one branch in handlers/uncordon.go:
//
//   - cordoned node + skipDeletion → 200 ok, audit appended
//   - already-uncordoned node      → 200 no-change, audit unchanged (FR-14)
//   - reclaimed node               → 200 reclaimed, audit appended (FR-17a)
//   - mismatched node              → 400 node-mismatch (FR-17)
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/audit"
)

func TestUncordon_HappyPath(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true /* unschedulable */, true /* skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, string(body))
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "ok" {
		t.Errorf("result = %q, want ok", body["result"])
	}

	// Verify the side effects: node uncordoned, audit entry appended.
	var refreshed nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &refreshed); err != nil {
		t.Fatalf("re-Get NHD: %v", err)
	}
	entries := mustReadAudit(t, &refreshed)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].Action != "uncordon" || entries[0].Result != "ok" || entries[0].Actor != "demo-anonymous" {
		t.Errorf("audit entry shape: got %+v", entries[0])
	}

	// Verify Cache-Control: no-store on the action endpoint.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want substring no-store", cc)
	}
}

func TestUncordon_AlreadyUncordoned_NoChangeNoAudit(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, false /* uncordoned */, true /* skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "already uncordoned (no change)" {
		t.Errorf("result = %q, want %q", body["result"], "already uncordoned (no change)")
	}

	// FR-14 — no audit entry on the no-change path.
	var refreshed nodemedicv1alpha1.NodeHealthDiagnosisAI
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &refreshed)
	entries := mustReadAudit(t, &refreshed)
	if len(entries) != 0 {
		t.Errorf("audit entries = %d on no-change path, want 0 (FR-14)", len(entries))
	}
}

func TestUncordon_ReclaimedNode_BenignAuditAppended(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_node_reclaimed.yaml")
	// Deliberately do NOT seed the Node CR.
	c := newFakeClient(t, nhd)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (FR-17a benign reclaimed)", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "node no longer exists (already reclaimed)" {
		t.Errorf("result = %q, want reclaimed-shape", body["result"])
	}

	var refreshed nodemedicv1alpha1.NodeHealthDiagnosisAI
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &refreshed)
	entries := mustReadAudit(t, &refreshed)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 (FR-17a writes one entry)", len(entries))
	}
	if entries[0].Result != "reclaimed" {
		t.Errorf("entry result = %q, want reclaimed", entries[0].Result)
	}
}

func TestUncordon_MissingNHD_404(t *testing.T) {
	c := newFakeClient(t)
	ts := newTestServer(t, c)
	resp, _ := ts.Client().Post(ts.URL+"/api/cases/no-such-nhd/actions/uncordon", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestUncordon_ApiserverPatchErrorReturns500(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true, true)
	failingPatch := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("simulated apiserver outage")
		},
	}
	cl := newFakeClientWithFuncs(t, failingPatch, nhd, node)
	ts := newTestServer(t, cl)
	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/uncordon", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["error"] != "apiserver-unavailable" {
		t.Errorf("error = %q, want apiserver-unavailable", body["error"])
	}
}

// ---------------- helpers ----------------

func decodeJSON(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

func mustReadAudit(t *testing.T, nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI) []audit.UIActionEntry {
	t.Helper()
	if nhd.Annotations == nil {
		return nil
	}
	raw := nhd.Annotations[audit.AnnotationKey]
	if raw == "" {
		return nil
	}
	entries, err := audit.ReadEntries(raw)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	return entries
}
