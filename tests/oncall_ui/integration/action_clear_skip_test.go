// T057 — clear-skip-deletion action integration tests. Mirror of
// T056 for the FR-15 endpoint.
package integration

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/render"
)

func TestClearSkip_HappyPath(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true /* unschedulable */, true /* skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/clear-skip-deletion", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "ok" {
		t.Errorf("result = %q, want ok", body["result"])
	}

	// Verify annotation removed
	var n nodemedicv1alpha1.NodeHealthDiagnosisAI
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &n)
	entries := mustReadAudit(t, &n)
	if len(entries) != 1 || entries[0].Action != "clear-skip-deletion" || entries[0].Result != "ok" {
		t.Errorf("audit entry shape: %+v", entries)
	}
	// Re-fetch the node to confirm the annotation is gone.
	var refreshedNode = struct{ NodeName string }{nhd.Spec.Case.NodeName}
	gotNode := nodeFor(refreshedNode.NodeName, false, false)
	if err := c.Get(context.Background(), types.NamespacedName{Name: refreshedNode.NodeName}, gotNode); err != nil {
		t.Fatalf("Get node: %v", err)
	}
	if _, has := gotNode.Annotations[render.MLCSkipDeletionAnnotation]; has {
		t.Errorf("skipDeletion annotation still present after clear")
	}
}

func TestClearSkip_AlreadyCleared_NoChangeNoAudit(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_applied.yaml")
	node := nodeFor(nhd.Spec.Case.NodeName, true /* unschedulable */, false /* skipDeletion absent */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/clear-skip-deletion", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "already cleared (no change)" {
		t.Errorf("result = %q, want %q", body["result"], "already cleared (no change)")
	}

	var n nodemedicv1alpha1.NodeHealthDiagnosisAI
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &n)
	if len(mustReadAudit(t, &n)) != 0 {
		t.Errorf("FR-15 — no audit entry on no-change path")
	}
}

func TestClearSkip_ReclaimedNode_BenignAuditAppended(t *testing.T) {
	nhd := loadNHDFixture(t, "nhd_node_reclaimed.yaml")
	c := newFakeClient(t, nhd)
	ts := newTestServer(t, c)

	resp, _ := ts.Client().Post(ts.URL+"/api/cases/"+nhd.Name+"/actions/clear-skip-deletion", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp.Body)
	if body["result"] != "node no longer exists (already reclaimed)" {
		t.Errorf("result = %q, want reclaimed shape", body["result"])
	}
	var n nodemedicv1alpha1.NodeHealthDiagnosisAI
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: nhd.Namespace, Name: nhd.Name}, &n)
	entries := mustReadAudit(t, &n)
	if len(entries) != 1 || entries[0].Result != "reclaimed" {
		t.Errorf("audit entry shape: %+v", entries)
	}
}
