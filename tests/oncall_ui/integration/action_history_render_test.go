// T085 (US5) — per-case page renders the merged controller+UI action
// history. Seeds an NHD that carries both status.action.decision (the
// controller half) and a ui-action-history annotation with two UI
// entries (the UI half). The rendered HTML must surface three rows in
// ts-ascending order.
package integration

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/audit"
)

func TestDetailHTML_MergedActionHistory(t *testing.T) {
	created := time.Date(2026, 6, 14, 15, 30, 0, 0, time.UTC)
	cordonTs := created.Add(30 * time.Second)    // controller cordon @ T+0:30
	uncordonTs := created.Add(60 * time.Second)  // UI uncordon  @ T+1:00
	clearSkipTs := created.Add(75 * time.Second) // UI clear     @ T+1:15

	entries := []audit.UIActionEntry{
		{Ts: uncordonTs, Action: "uncordon", Actor: "demo-anonymous", Result: "ok"},
		{Ts: clearSkipTs, Action: "clear-skip-deletion", Actor: "demo-anonymous", Result: "ok"},
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode entries: %v", err)
	}

	appliedAt := metav1.NewTime(cordonTs)
	completed := metav1.NewTime(created.Add(20 * time.Second))
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cf1z-general-nodes-2000001-1718374200",
			Namespace:         "cf-monitoring",
			CreationTimestamp: metav1.NewTime(created),
			UID:               "22222222-2222-4222-8222-222222222222",
			Annotations: map[string]string{
				audit.AnnotationKey: string(encoded),
			},
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				NodeName:    "cf1z-general-nodes-2000001",
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAzure,
				Region:      "eastus2",
				InstanceId:  "cf1z-general-nodes-2000001-vm",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "KubeletUnhealthy",
					Reason:     "KubeletNotReady",
					Message:    "kubelet not ready",
					ObservedAt: metav1.NewTime(created.Add(-10 * time.Second)),
				},
			},
		},
		Status: nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
			Phase: nodemedicv1alpha1.PhaseActed,
			Diagnosis: &nodemedicv1alpha1.Diagnosis{
				RootCause:   "kubelet unhealthy",
				RCACategory: nodemedicv1alpha1.RCACategory("Kubelet"),
				Confidence:  0.92,
				ModelUsed:   "claude-haiku-4-5",
				CompletedAt: &completed,
			},
			Action: &nodemedicv1alpha1.ActionStatus{
				Decision:  nodemedicv1alpha1.ActionDecisionApplied,
				Operation: nodemedicv1alpha1.ActionOperationCordon,
				AppliedAt: &appliedAt,
			},
		},
	}
	// After the engineer's clear-skip-deletion, the node is uncordoned
	// and the annotation is gone — that's what the live cluster looks
	// like at this point in time.
	node := nodeFor(nhd.Spec.Case.NodeName, false /* uncordoned */, false /* no skipDeletion */)
	c := newFakeClient(t, nhd, node)
	ts := newTestServer(t, c)

	resp, err := ts.Client().Get(ts.URL + "/cases/" + nhd.Name)
	if err != nil {
		t.Fatalf("GET /cases/%s: %v", nhd.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	// Each verb + the two distinct actor strings must appear.
	for _, want := range []string{"controller", "UI: demo-anonymous", "Cordon", "Uncordon", "ClearSkipDeletion"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}

	// Order check: ts-ascending → first occurrence of Cordon precedes
	// Uncordon precedes ClearSkipDeletion. Substring index suffices —
	// the rows are emitted in <li> elements in template order.
	idxCordon := strings.Index(s, "Cordon")
	idxUncordon := strings.Index(s, "Uncordon")
	idxClearSkip := strings.Index(s, "ClearSkipDeletion")
	if idxCordon < 0 || idxUncordon < 0 || idxClearSkip < 0 {
		t.Fatalf("verb tokens missing: cordon=%d uncordon=%d clearSkip=%d", idxCordon, idxUncordon, idxClearSkip)
	}
	if !(idxCordon < idxUncordon && idxUncordon < idxClearSkip) {
		t.Errorf("rows not in ts-ascending order: cordon@%d uncordon@%d clearSkip@%d", idxCordon, idxUncordon, idxClearSkip)
	}

	// Two "UI: demo-anonymous" actors must appear (one per UI entry).
	if got := strings.Count(s, "UI: demo-anonymous"); got != 2 {
		t.Errorf("count(\"UI: demo-anonymous\") = %d, want 2", got)
	}
}
