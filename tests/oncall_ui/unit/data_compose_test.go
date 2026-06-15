// T037 — pure-function tests for composeListPageRow and
// ComposeDetailPageData per data-model.md §4 + §5.
//
// FR-9a (button enablement gated by node state, not phase) is the
// primary invariant under test; each fixture combo (cordoned vs
// uncordoned, skip-deletion stamped vs not, reclaimed) flips a
// known boolean and the test asserts the resulting struct.
//
// T084 (US5) extends this file with composeActionHistory fixtures
// that exercise the merged controller+UI history path (data-model.md
// §4 composition rules).
package unit

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/audit"
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

// ---------------------------------------------------------------------
// T084 (US5) — composeActionHistory merge fixtures
//
// data-model.md §4 composition rules:
//   - controller's status.action (decision != "") → row {Actor:
//     "controller", ActionVerb: "Cordon", Ts: appliedAt, Result: decision}
//   - each ui-action-history annotation entry → row {Actor: "UI: " +
//     entry.Actor, ActionVerb: verb-map(entry.Action), Ts: entry.Ts,
//     Result: entry.Result + (" — " + entry.Detail if non-empty)}
//   - sort by Ts ascending; engineer reads top-to-bottom
// ---------------------------------------------------------------------

// TestComposeActionHistory_ControllerOnly is the Phase 4 baseline path:
// controller has acted, UI has not — exactly one row.
func TestComposeActionHistory_ControllerOnly(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-controller-only", "node-a", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	// No annotation set; explicitly nil so the read path exercises the
	// "annotations map is nil" branch the live cf1z NHDs hit pre-Phase-5.
	nhd.Annotations = nil

	d := render.ComposeDetailPageData(&nhd, makeNode("node-a", true, true), false)

	if len(d.ActionHistory) != 1 {
		t.Fatalf("ActionHistory length = %d, want 1 (controller-only path)", len(d.ActionHistory))
	}
	row := d.ActionHistory[0]
	if row.Actor != "controller" {
		t.Errorf("row.Actor = %q, want %q", row.Actor, "controller")
	}
	if row.ActionVerb != "Cordon" {
		t.Errorf("row.ActionVerb = %q, want %q", row.ActionVerb, "Cordon")
	}
	if row.Result != string(nodemedicv1alpha1.ActionDecisionApplied) {
		t.Errorf("row.Result = %q, want %q", row.Result, nodemedicv1alpha1.ActionDecisionApplied)
	}
	// Ts comes from status.action.appliedAt (created+2m per makeNHD).
	wantTs := now.Add(-30*time.Minute + 2*time.Minute)
	if !row.Ts.Equal(wantTs) {
		t.Errorf("row.Ts = %v, want %v", row.Ts, wantTs)
	}
}

// TestComposeActionHistory_ControllerPlusTwoUIEntries is the demo
// finale shape — engineer ran uncordon and drain after the controller
// cordoned. Three rows must be sorted ts-ascending: Cordon, Uncordon,
// Drain.
func TestComposeActionHistory_ControllerPlusTwoUIEntries(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-controller-plus-two", "node-b", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	// Controller's appliedAt is now-28m (created+2m). UI entries land
	// at now-27m (uncordon) and now-26m (drain) — ts-ascending order
	// is Cordon → Uncordon → Drain.
	uncordonTs := now.Add(-27 * time.Minute)
	drainTs := now.Add(-26 * time.Minute)
	entries := []audit.UIActionEntry{
		{Ts: uncordonTs, Action: "uncordon", Actor: "demo-anonymous", Result: "ok"},
		{Ts: drainTs, Action: "drain", Actor: "demo-anonymous", Result: "partial", Detail: "evicted=12 skipped=3 errored=1"},
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode entries: %v", err)
	}
	nhd.Annotations = map[string]string{audit.AnnotationKey: string(encoded)}

	d := render.ComposeDetailPageData(&nhd, makeNode("node-b", true, true), false)

	if len(d.ActionHistory) != 3 {
		t.Fatalf("ActionHistory length = %d, want 3 (controller + 2 UI)", len(d.ActionHistory))
	}
	want := []struct {
		actor, verb string
	}{
		{"controller", "Cordon"},
		{"UI: demo-anonymous", "Uncordon"},
		{"UI: demo-anonymous", "Drain"},
	}
	for i, w := range want {
		got := d.ActionHistory[i]
		if got.Actor != w.actor {
			t.Errorf("row[%d].Actor = %q, want %q", i, got.Actor, w.actor)
		}
		if got.ActionVerb != w.verb {
			t.Errorf("row[%d].ActionVerb = %q, want %q", i, got.ActionVerb, w.verb)
		}
	}
	// Verify ts-ascending invariant explicitly so a future composition-
	// rule regression doesn't slip through name/verb assertions alone.
	for i := 1; i < len(d.ActionHistory); i++ {
		if d.ActionHistory[i].Ts.Before(d.ActionHistory[i-1].Ts) {
			t.Errorf("rows not ts-ascending at i=%d: %v before %v", i, d.ActionHistory[i].Ts, d.ActionHistory[i-1].Ts)
		}
	}
	// Drain row's Result must carry the detail summary (composition
	// rule: "Result + ' — ' + Detail if non-empty").
	drainRow := d.ActionHistory[2]
	if drainRow.Result != "partial — evicted=12 skipped=3 errored=1" {
		t.Errorf("drain row Result = %q, want %q", drainRow.Result, "partial — evicted=12 skipped=3 errored=1")
	}
}

// TestComposeActionHistory_UIOnly is the pre-controller-action edge
// case — the engineer somehow took action before status.action was
// stamped. Should produce a single UI-actor row with no controller
// row leaking in.
func TestComposeActionHistory_UIOnly(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-ui-only", "node-c", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseDiagnosed, "", 0)
	nhd.Status.Action = nil

	clearTs := now.Add(-25 * time.Minute)
	entries := []audit.UIActionEntry{
		{Ts: clearTs, Action: "clear-skip-deletion", Actor: "demo-anonymous", Result: "ok"},
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode entries: %v", err)
	}
	nhd.Annotations = map[string]string{audit.AnnotationKey: string(encoded)}

	d := render.ComposeDetailPageData(&nhd, makeNode("node-c", true, false), false)

	if len(d.ActionHistory) != 1 {
		t.Fatalf("ActionHistory length = %d, want 1 (UI-only path)", len(d.ActionHistory))
	}
	row := d.ActionHistory[0]
	if row.Actor != "UI: demo-anonymous" {
		t.Errorf("row.Actor = %q, want %q", row.Actor, "UI: demo-anonymous")
	}
	if row.ActionVerb != "ClearSkipDeletion" {
		t.Errorf("row.ActionVerb = %q, want %q", row.ActionVerb, "ClearSkipDeletion")
	}
	if row.Result != "ok" {
		t.Errorf("row.Result = %q, want %q (no detail → no separator)", row.Result, "ok")
	}
	if !row.Ts.Equal(clearTs) {
		t.Errorf("row.Ts = %v, want %v", row.Ts, clearTs)
	}
}

// TestComposeActionHistory_MalformedAnnotationFallsThroughToController
// is the "tolerant decoder" guarantee — a malformed annotation value
// (e.g. truncated bytes from a buggy past write) must NOT cause the
// per-case page to lose the controller's history row. The annotation
// is dropped; the controller half is still rendered.
func TestComposeActionHistory_MalformedAnnotationFallsThroughToController(t *testing.T) {
	now := time.Date(2026, 6, 14, 16, 0, 0, 0, time.UTC)
	nhd := makeNHD("nhd-malformed", "node-d", now.Add(-30*time.Minute), nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.ActionDecisionApplied, 0.92)
	nhd.Annotations = map[string]string{audit.AnnotationKey: `[{"ts":"`} // truncated JSON

	d := render.ComposeDetailPageData(&nhd, makeNode("node-d", true, true), false)

	if len(d.ActionHistory) != 1 {
		t.Fatalf("ActionHistory length = %d, want 1 (malformed annotation should not break the controller half)", len(d.ActionHistory))
	}
	if d.ActionHistory[0].Actor != "controller" {
		t.Errorf("row[0].Actor = %q, want %q", d.ActionHistory[0].Actor, "controller")
	}
}
