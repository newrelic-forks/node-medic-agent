// Package render carries the on-call UI's html/template surface and
// the data shapes (ListPageRow, DetailPageData, ActionHistoryRow) that
// the templates consume. Templates and static assets ship as embed.FS
// from this package.
//
// The composition functions in this file (composeListPageRow,
// composeDetailPageData) are pure: given a NHD CR and an optional
// Node CR, they return the template input. No kube-client calls
// happen here — the handler layer (internal/oncall/handlers/) does
// the apiserver round-trips, then hands typed values in.
package render

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// MLCSkipDeletionAnnotation is the Node annotation key the Spec 001
// controller stamps on cordon to pause MLC reclaim. The UI's clear-
// skip-deletion action removes this key.
const MLCSkipDeletionAnnotation = "machine-lifecycle.newrelic.com/skipDeletion"

// EvidenceCollapseThreshold is the byte length above which an evidence
// entry's body is collapsed by default in the per-case page (FR-10).
const EvidenceCollapseThreshold = 800

// ConfidenceUnset is the sentinel returned by composeListPageRow when
// the NHD has no diagnosis yet — lets the template render "—" instead
// of "0.00" so the engineer knows the agent hasn't written anything.
const ConfidenceUnset float64 = -1

// ListPageRow is one row in the list view. The /api/cases JSON
// endpoint marshals []ListPageRow directly so the SSR template and the
// auto-refresh consumer see identical bytes.
//
// Field tags match contracts/oncall-ui-api.yaml's ListPageRow schema.
type ListPageRow struct {
	NHDName       string    `json:"nhdName"`
	NodeName      string    `json:"nodeName"`
	ClusterName   string    `json:"clusterName"`
	CreatedAt     time.Time `json:"createdAt"`
	TriggerType   string    `json:"triggerType"`
	TriggerReason string    `json:"triggerReason,omitempty"`
	Phase         string    `json:"phase"`
	Decision      string    `json:"decision"`
	Confidence    float64   `json:"confidence"`
	RowClass      string    `json:"rowClass"`
}

// DetailPageData drives the per-case page template. Computed from one
// NHD Get + one Node Get (or NodeName lookup that 404s).
type DetailPageData struct {
	NHDName     string
	Namespace   string
	CaseID      string
	NodeName    string
	ClusterName string
	Provider    string
	Region      string
	InstanceID  string

	Trigger TriggerView

	Phase           string
	PhaseBannerKind string // "" | "in-progress" | "failed" — drives status banner CSS class

	HasDiagnosis bool
	Diagnosis    DiagnosisView

	ActionHistory []ActionHistoryRow

	NodeExists                      bool
	NodeUnschedulable               bool
	NodeHasSkipDeletion             bool
	UncordonEnabled                 bool
	UncordonDisabledReason          string
	ClearSkipDeletionEnabled        bool
	ClearSkipDeletionDisabledReason string
	DrainEnabled                    bool
	DrainDisabledReason             string

	// DrainPlanPods is the result of the drain pre-flight pod scan
	// (research R-3). Present only when DrainEnabled is true and the
	// detail handler scanned the node's pods at request time. Used by
	// the confirmation modal in Phase 6 (US4).
	DrainPlanPods []DrainPlanPod
}

// TriggerView is the render-time shape of the NHD's case trigger.
type TriggerView struct {
	Type       string
	Reason     string
	Message    string
	ObservedAt time.Time
}

// DiagnosisView is the render-time shape of the agent's diagnosis. A
// zero value means the agent has not yet written.
type DiagnosisView struct {
	RootCause      string
	RCACategory    string
	Confidence     float64
	Evidence       []EvidenceView
	Recommendation RecommendationView
	ModelUsed      string
	CompletedAt    time.Time
}

// EvidenceView is one collapsible evidence card.
type EvidenceView struct {
	Source      string
	Ref         string
	Result      string
	ObservedAt  time.Time
	Collapsible bool
}

// RecommendationView is the agent's recommended action (display-only).
type RecommendationView struct {
	Action string
	Reason string
}

// ActionHistoryRow is one merged controller+UI history row. Sorted by
// Ts ascending (engineer reads top-to-bottom). data-model.md §4
// composition rules.
type ActionHistoryRow struct {
	Ts         time.Time
	Actor      string
	ActionVerb string
	Result     string
}

// DrainPlanPod is one entry in the drain confirmation modal's pod list.
// Same shape Phase 6 will surface; a zero-length slice is fine when
// the drain pre-flight wasn't run.
type DrainPlanPod struct {
	Namespace string
	Name      string
	Eligible  bool   // true → would be evicted; false → would be skipped
	Reason    string // "DaemonSet" | "mirror" | "system-node-critical" | "terminating" | ""
}

// composeListPageRow folds an NHD CR into a ListPageRow. Pure: no
// kube-client side effects. Sort/filter happens at the caller.
func composeListPageRow(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI) ListPageRow {
	row := ListPageRow{
		NHDName:       nhd.Name,
		NodeName:      nhd.Spec.Case.NodeName,
		ClusterName:   nhd.Spec.Case.ClusterName,
		CreatedAt:     nhd.CreationTimestamp.Time,
		TriggerType:   nhd.Spec.Case.Trigger.Type,
		TriggerReason: nhd.Spec.Case.Trigger.Reason,
		Phase:         string(nhd.Status.Phase),
		Confidence:    ConfidenceUnset,
	}
	if nhd.Status.Diagnosis != nil {
		// Confidence==0 with a non-nil Diagnosis is still a real value
		// (the agent wrote a confidence of 0). Only the no-diagnosis
		// branch keeps the -1 sentinel.
		row.Confidence = nhd.Status.Diagnosis.Confidence
	}
	if nhd.Status.Action != nil {
		row.Decision = string(nhd.Status.Action.Decision)
	}
	row.RowClass = rowClassFor(nhd)
	return row
}

// rowClassFor implements FR-8 highlighting:
//
//   - row-applied      — phase=Acted + decision=Applied
//   - row-human-in-loop — phase=Acted + decision=HumanInLoop
//   - row-failed       — phase=Failed
//   - row-neutral      — any other phase (Pending, Diagnosing, Diagnosed,
//     plus Acted+decision="" which can occur transiently)
func rowClassFor(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI) string {
	if nhd.Status.Phase == nodemedicv1alpha1.PhaseFailed {
		return "row-failed"
	}
	if nhd.Status.Phase == nodemedicv1alpha1.PhaseActed && nhd.Status.Action != nil {
		switch nhd.Status.Action.Decision {
		case nodemedicv1alpha1.ActionDecisionApplied:
			return "row-applied"
		case nodemedicv1alpha1.ActionDecisionHumanInLoop:
			return "row-human-in-loop"
		}
	}
	return "row-neutral"
}

// ComposeListRows builds the slice the list page template iterates
// over. Filters to creationTimestamp >= now-24h (FR-5), sorts
// newest first.
func ComposeListRows(items []nodemedicv1alpha1.NodeHealthDiagnosisAI, now time.Time) []ListPageRow {
	cutoff := now.Add(-24 * time.Hour)
	rows := make([]ListPageRow, 0, len(items))
	for i := range items {
		if items[i].CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		rows = append(rows, composeListPageRow(&items[i]))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	return rows
}

// ComposeDetailPageData folds one NHD + (optional) Node into the per-
// case page input. nodeNotFound==true means the kube Get returned
// k8s.io/apimachinery NotFound — drives the FR-17a reclaimed-node
// banner and disables all three buttons.
//
// Pure (modulo time.Time fields preserved as-is): no kube-client
// round-trips here. Caller passes node==nil + nodeNotFound==true for
// the reclaimed branch.
func ComposeDetailPageData(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI, node *corev1.Node, nodeNotFound bool) DetailPageData {
	d := DetailPageData{
		NHDName:     nhd.Name,
		Namespace:   nhd.Namespace,
		CaseID:      nhd.Spec.Case.CaseId,
		NodeName:    nhd.Spec.Case.NodeName,
		ClusterName: nhd.Spec.Case.ClusterName,
		Provider:    string(nhd.Spec.Case.Provider),
		Region:      nhd.Spec.Case.Region,
		InstanceID:  nhd.Spec.Case.InstanceId,
		Trigger: TriggerView{
			Type:       nhd.Spec.Case.Trigger.Type,
			Reason:     nhd.Spec.Case.Trigger.Reason,
			Message:    nhd.Spec.Case.Trigger.Message,
			ObservedAt: nhd.Spec.Case.Trigger.ObservedAt.Time,
		},
		Phase: string(nhd.Status.Phase),
	}

	d.PhaseBannerKind = phaseBannerKind(nhd.Status.Phase)

	if nhd.Status.Diagnosis != nil {
		d.HasDiagnosis = true
		d.Diagnosis = composeDiagnosisView(nhd.Status.Diagnosis)
	}

	d.ActionHistory = composeActionHistory(nhd)

	// Live node state drives button enablement (FR-9a). Reclaimed
	// branch (nodeNotFound==true) wins over every other consideration.
	if nodeNotFound {
		d.NodeExists = false
		d.UncordonEnabled = false
		d.UncordonDisabledReason = "Node was reclaimed by MLC"
		d.ClearSkipDeletionEnabled = false
		d.ClearSkipDeletionDisabledReason = "Node was reclaimed by MLC"
		d.DrainEnabled = false
		d.DrainDisabledReason = "Node was reclaimed by MLC"
		return d
	}
	if node == nil {
		// Defensive: caller didn't supply a node and didn't flag it
		// reclaimed. Treat as "node state unknown" — disable buttons
		// rather than emit a false-positive enabled state.
		d.NodeExists = false
		d.UncordonEnabled = false
		d.UncordonDisabledReason = "Node state unavailable"
		d.ClearSkipDeletionEnabled = false
		d.ClearSkipDeletionDisabledReason = "Node state unavailable"
		d.DrainEnabled = false
		d.DrainDisabledReason = "Node state unavailable"
		return d
	}

	d.NodeExists = true
	d.NodeUnschedulable = node.Spec.Unschedulable
	if node.Annotations != nil {
		_, hasSkip := node.Annotations[MLCSkipDeletionAnnotation]
		d.NodeHasSkipDeletion = hasSkip
	}

	// Uncordon enabled iff cordoned. Otherwise tooltip carries the
	// reason the engineer sees on hover.
	if d.NodeUnschedulable {
		d.UncordonEnabled = true
	} else {
		d.UncordonDisabledReason = "Already uncordoned"
	}
	if d.NodeHasSkipDeletion {
		d.ClearSkipDeletionEnabled = true
	} else {
		d.ClearSkipDeletionDisabledReason = "Annotation already cleared"
	}
	// Drain only requires the node to exist (FR-9a). The pre-flight
	// pod-list filter happens at action time.
	d.DrainEnabled = true

	return d
}

// phaseBannerKind maps an NHD phase to the banner CSS class slug.
// Empty string means no banner (terminal phases that have full data).
func phaseBannerKind(phase nodemedicv1alpha1.Phase) string {
	switch phase {
	case nodemedicv1alpha1.PhasePending, nodemedicv1alpha1.PhaseDiagnosing:
		return "in-progress"
	case nodemedicv1alpha1.PhaseFailed:
		return "failed"
	default:
		return ""
	}
}

// composeDiagnosisView folds the agent's diagnosis into the render
// shape. Evidence longer than EvidenceCollapseThreshold bytes is
// collapsed by default per FR-10.
func composeDiagnosisView(diag *nodemedicv1alpha1.Diagnosis) DiagnosisView {
	view := DiagnosisView{
		RootCause:   diag.RootCause,
		RCACategory: string(diag.RCACategory),
		Confidence:  diag.Confidence,
		ModelUsed:   diag.ModelUsed,
	}
	if diag.CompletedAt != nil {
		view.CompletedAt = diag.CompletedAt.Time
	}
	view.Evidence = make([]EvidenceView, 0, len(diag.Evidence))
	for _, e := range diag.Evidence {
		ev := EvidenceView{
			Source:      string(e.Source),
			Ref:         e.Ref,
			Result:      e.Result,
			Collapsible: len(e.Result) > EvidenceCollapseThreshold,
		}
		if e.ObservedAt != nil {
			ev.ObservedAt = e.ObservedAt.Time
		}
		view.Evidence = append(view.Evidence, ev)
	}
	if diag.Recommendation != nil {
		view.Recommendation = RecommendationView{
			Action: string(diag.Recommendation.Action),
			Reason: diag.Recommendation.Reason,
		}
	}
	return view
}

// composeActionHistory wires the controller's status.action into the
// merged history. Phase 4 (T040) ships the controller half only; Phase
// 7 (T086, US5) extends the function to decode the ui-action-history
// annotation entries and merge them. The signature is the same in
// both phases — callers in Phase 4 only see one row max.
//
// Bound by data-model.md §4 composition rules.
func composeActionHistory(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI) []ActionHistoryRow {
	rows := make([]ActionHistoryRow, 0, 4)
	if nhd.Status.Action != nil && nhd.Status.Action.Decision != "" {
		ts := time.Time{}
		if nhd.Status.Action.AppliedAt != nil {
			ts = nhd.Status.Action.AppliedAt.Time
		}
		rows = append(rows, ActionHistoryRow{
			Ts:         ts,
			Actor:      "controller",
			ActionVerb: "Cordon",
			Result:     string(nhd.Status.Action.Decision),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Ts.Before(rows[j].Ts)
	})
	return rows
}
