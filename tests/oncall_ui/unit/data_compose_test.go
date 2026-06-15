// T037 — pure-function tests for composeListPageRow and
// ComposeDetailPageData per data-model.md §4 + §5.
//
// FR-9a (button enablement gated by node state, not phase) is the
// primary invariant under test; each fixture combo (cordoned vs
// uncordoned, skip-deletion stamped vs not, reclaimed) flips a
// known boolean and the test asserts the resulting struct.
package unit

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/render"
)

func TestComposeListRows_FilterAndSort(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)

	items := []nodemedicv1alpha1.NodeHealthDiagnosisAI{
		makeNHD("nhd-old-25h", "node-a", now.Add(-25*time.Hour), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.9),
		makeNHD("nhd-recent", "node-b", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.9),
		makeNHD("nhd-newer", "node-c", now.Add(-5*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionHumanInLoop, 0.4),
		makeNHD("nhd-failed", "node-d", now.Add(-1*time.Hour), nodemedicv1alpha1.PhaseFailed, "", 0),
	}

	rows := render.ComposeListRows(items, now)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (24h filter drops nhd-old-25h), got %d", len(rows))
	}
	wantOrder := []string{"nhd-newer", "nhd-recent", "nhd-failed"}
	for i, w := range wantOrder {
		if rows[i].NHDName != w {
			t.Errorf("rows[%d].NHDName = %q, want %q", i, rows[i].NHDName, w)
		}
	}

	// Row class drives FR-8 highlighting.
	classByName := map[string]string{}
	for _, r := range rows {
		classByName[r.NHDName] = r.RowClass
	}
	if got, want := classByName["nhd-recent"], "row-applied"; got != want {
		t.Errorf("nhd-recent rowClass = %q, want %q", got, want)
	}
	if got, want := classByName["nhd-newer"], "row-human-in-loop"; got != want {
		t.Errorf("nhd-newer rowClass = %q, want %q", got, want)
	}
	if got, want := classByName["nhd-failed"], "row-failed"; got != want {
		t.Errorf("nhd-failed rowClass = %q, want %q", got, want)
	}
}

func TestComposeListRows_ConfidenceSentinelWhenNoDiagnosis(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-pending", "node-x", now.Add(-1*time.Minute), nodemedicv1alpha1.PhaseDiagnosing, "", 0)
	nhd.Status.Diagnosis = nil

	rows := render.ComposeListRows([]nodemedicv1alpha1.NodeHealthDiagnosisAI{nhd}, now)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Confidence != render.ConfidenceUnset {
		t.Errorf("Confidence = %v, want sentinel %v", rows[0].Confidence, render.ConfidenceUnset)
	}
	if rows[0].RowClass != "row-neutral" {
		t.Errorf("RowClass = %q, want row-neutral for Diagnosing", rows[0].RowClass)
	}
}

func TestComposeDetailPageData_AppliedHappyPath(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-applied", "node-applied", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	node := makeNode("node-applied", true /* unschedulable */, true /* skipDeletion */)

	d := render.ComposeDetailPageData(&nhd, node, false)

	if !d.NodeExists {
		t.Errorf("NodeExists = false, want true")
	}
	if !d.UncordonEnabled {
		t.Errorf("UncordonEnabled = false, want true (node is cordoned)")
	}
	if !d.ClearSkipDeletionEnabled {
		t.Errorf("ClearSkipDeletionEnabled = false, want true (annotation present)")
	}
	if !d.DrainEnabled {
		t.Errorf("DrainEnabled = false, want true (node exists)")
	}
	if d.PhaseBannerKind != "" {
		t.Errorf("PhaseBannerKind = %q, want empty for Acted", d.PhaseBannerKind)
	}
	if !d.HasDiagnosis {
		t.Errorf("HasDiagnosis = false, want true")
	}
	if got, want := d.Diagnosis.Confidence, 0.92; got != want {
		t.Errorf("Confidence = %v, want %v", got, want)
	}
	if len(d.ActionHistory) != 1 || d.ActionHistory[0].Actor != "controller" || d.ActionHistory[0].ActionVerb != "Cordon" {
		t.Errorf("ActionHistory = %#v, want one controller Cordon row", d.ActionHistory)
	}
}

func TestComposeDetailPageData_UncordonedNode_DisablesUncordon(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-foo", "node-foo", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	node := makeNode("node-foo", false /* unschedulable */, true /* skipDeletion */)

	d := render.ComposeDetailPageData(&nhd, node, false)

	if d.UncordonEnabled {
		t.Errorf("UncordonEnabled = true, want false (node already uncordoned)")
	}
	if d.UncordonDisabledReason != "Already uncordoned" {
		t.Errorf("UncordonDisabledReason = %q, want %q", d.UncordonDisabledReason, "Already uncordoned")
	}
	if !d.ClearSkipDeletionEnabled {
		t.Errorf("ClearSkipDeletionEnabled = false, want true (annotation still present)")
	}
}

func TestComposeDetailPageData_NoSkipDeletion_DisablesClearSkip(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-foo", "node-foo", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	node := makeNode("node-foo", true /* unschedulable */, false /* skipDeletion */)

	d := render.ComposeDetailPageData(&nhd, node, false)

	if d.ClearSkipDeletionEnabled {
		t.Errorf("ClearSkipDeletionEnabled = true, want false (annotation absent)")
	}
	if d.ClearSkipDeletionDisabledReason != "Annotation already cleared" {
		t.Errorf("ClearSkipDeletionDisabledReason = %q, want %q", d.ClearSkipDeletionDisabledReason, "Annotation already cleared")
	}
}

func TestComposeDetailPageData_DiagnosingPhase_BannerInProgress(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-pending", "node-pending", now.Add(-30*time.Second), nodemedicv1alpha1.PhaseDiagnosing, "", 0)
	nhd.Status.Diagnosis = nil
	nhd.Status.Action = nil
	node := makeNode("node-pending", true, true)

	d := render.ComposeDetailPageData(&nhd, node, false)

	if d.PhaseBannerKind != "in-progress" {
		t.Errorf("PhaseBannerKind = %q, want %q", d.PhaseBannerKind, "in-progress")
	}
	if d.HasDiagnosis {
		t.Errorf("HasDiagnosis = true, want false")
	}
	// FR-9a: phase banner does NOT gate buttons.
	if !d.UncordonEnabled {
		t.Errorf("Uncordon must stay enabled during Diagnosing if node is cordoned (FR-9a)")
	}
	if !d.ClearSkipDeletionEnabled {
		t.Errorf("ClearSkipDeletion must stay enabled during Diagnosing if annotation is present (FR-9a)")
	}
}

func TestComposeDetailPageData_ReclaimedNode_DisablesAllButtons(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-gone", "node-gone", now.Add(-1*time.Hour), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.85)

	d := render.ComposeDetailPageData(&nhd, nil, true /* nodeNotFound */)

	if d.NodeExists {
		t.Errorf("NodeExists = true, want false")
	}
	if d.UncordonEnabled || d.ClearSkipDeletionEnabled || d.DrainEnabled {
		t.Errorf("all three buttons must be disabled when node is reclaimed; got uncordon=%v clearSkip=%v drain=%v",
			d.UncordonEnabled, d.ClearSkipDeletionEnabled, d.DrainEnabled)
	}
	if d.UncordonDisabledReason != "Node was reclaimed by MLC" {
		t.Errorf("UncordonDisabledReason = %q, want %q", d.UncordonDisabledReason, "Node was reclaimed by MLC")
	}
}

func TestComposeDetailPageData_FailedPhase_BannerFailed(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-failed", "node-z", now.Add(-1*time.Hour), nodemedicv1alpha1.PhaseFailed, "", 0)
	nhd.Status.Diagnosis = nil
	nhd.Status.Action = nil
	node := makeNode("node-z", true, false)

	d := render.ComposeDetailPageData(&nhd, node, false)
	if d.PhaseBannerKind != "failed" {
		t.Errorf("PhaseBannerKind = %q, want %q", d.PhaseBannerKind, "failed")
	}
}

func TestComposeDetailPageData_EvidenceCollapsibleByLength(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-evi", "node-evi", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	short := nodemedicv1alpha1.EvidenceItem{Source: "kubectl", Ref: "kubectl get node", Result: "short result"}
	long := nodemedicv1alpha1.EvidenceItem{Source: "ssh", Ref: "journalctl", Result: makeRunes('x', render.EvidenceCollapseThreshold+1)}
	nhd.Status.Diagnosis.Evidence = []nodemedicv1alpha1.EvidenceItem{short, long}

	d := render.ComposeDetailPageData(&nhd, makeNode("node-evi", true, true), false)

	if len(d.Diagnosis.Evidence) != 2 {
		t.Fatalf("expected 2 evidence entries, got %d", len(d.Diagnosis.Evidence))
	}
	if d.Diagnosis.Evidence[0].Collapsible {
		t.Errorf("short evidence Collapsible = true, want false")
	}
	if !d.Diagnosis.Evidence[1].Collapsible {
		t.Errorf("long evidence Collapsible = false, want true (len > %d)", render.EvidenceCollapseThreshold)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func makeNHD(name, node string, created time.Time, phase nodemedicv1alpha1.Phase, decision nodemedicv1alpha1.ActionDecision, confidence float64) nodemedicv1alpha1.NodeHealthDiagnosisAI {
	rec := &nodemedicv1alpha1.Recommendation{Action: nodemedicv1alpha1.RecommendationActionCordon, Reason: "kubelet unhealthy"}
	completed := metav1.NewTime(created.Add(1 * time.Minute))
	applied := metav1.NewTime(created.Add(2 * time.Minute))
	nhd := nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "cf-monitoring",
			CreationTimestamp: metav1.NewTime(created),
			UID:               "00000000-0000-4000-8000-000000000000",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "00000000-0000-4000-8000-000000000000",
				NodeName:    node,
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAzure,
				Region:      "eastus2",
				InstanceId:  node + "-vm",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "KubeletUnhealthy",
					Reason:     "KubeletNotReady",
					Message:    "kubelet not ready",
					ObservedAt: metav1.NewTime(created.Add(-10 * time.Second)),
				},
			},
		},
		Status: nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
			Phase: phase,
			Diagnosis: &nodemedicv1alpha1.Diagnosis{
				RootCause:      "kubelet unhealthy",
				RCACategory:    nodemedicv1alpha1.RCACategory("Kubelet"),
				Confidence:     confidence,
				Recommendation: rec,
				ModelUsed:      "claude-haiku-4-5",
				CompletedAt:    &completed,
			},
		},
	}
	if decision != "" {
		nhd.Status.Action = &nodemedicv1alpha1.ActionStatus{
			Decision:  decision,
			Operation: nodemedicv1alpha1.ActionOperationCordon,
			AppliedAt: &applied,
		}
	}
	return nhd
}

func makeNode(name string, unschedulable, skipDeletion bool) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
	if skipDeletion {
		n.Annotations = map[string]string{render.MLCSkipDeletionAnnotation: "true"}
	}
	return n
}

func makeRunes(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
