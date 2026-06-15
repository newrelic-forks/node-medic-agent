// Package drain ships the on-call UI's drain action: pre-flight pod
// list filtering, in-flight progress tracking, and the SSE-streamed
// eviction loop.
package drain

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PriorityClassSystemNodeCritical is the kube-builtin priority class
// name above which a pod is "system-critical" — drain skips it
// (research R-3). The k8s.io/api/scheduling/v1 package documents but
// does not export this name as a constant, so we redeclare it here.
const PriorityClassSystemNodeCritical = "system-node-critical"

// SkipReason is the eviction-skip reason the SSE consumer sees in
// the per-pod `detail` field.
type SkipReason string

const (
	SkipDaemonSet          SkipReason = "DaemonSet"
	SkipMirror             SkipReason = "mirror"
	SkipSystemNodeCritical SkipReason = "system-node-critical"
	SkipTerminating        SkipReason = "terminating"
)

// PodShouldEvict returns (true, "") if the pod is eligible for
// eviction; otherwise (false, reason) where reason names the filter
// that excluded it. Pure: caller has already fetched the pod.
//
// Filter rules per research R-3:
//   - DaemonSet pods (any owner ref of kind=DaemonSet)
//   - mirror pods (annotation kubernetes.io/config.mirror)
//   - system-node-critical priority
//   - terminating pods (deletionTimestamp != nil)
func PodShouldEvict(pod *corev1.Pod) (bool, SkipReason) {
	if pod == nil {
		return false, ""
	}
	if pod.DeletionTimestamp != nil {
		return false, SkipTerminating
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return false, SkipDaemonSet
		}
	}
	if pod.Annotations != nil {
		if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
			return false, SkipMirror
		}
	}
	if pod.Spec.PriorityClassName == PriorityClassSystemNodeCritical {
		return false, SkipSystemNodeCritical
	}
	return true, ""
}

// PodDecision is the result of running PodShouldEvict over one pod;
// the eviction loop iterates over a slice of these in arrival order.
type PodDecision struct {
	Pod      *corev1.Pod
	Eligible bool
	Reason   SkipReason
}

// PlanForNode lists all pods on nodeName via an apiserver-side field
// selector (no client-side cache — research R-5 / FR-24) and folds
// each into a PodDecision. Returns the decisions in the apiserver's
// natural order.
func PlanForNode(ctx context.Context, c client.Client, nodeName string) ([]PodDecision, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", nodeName),
	}); err != nil {
		return nil, err
	}
	out := make([]PodDecision, 0, len(podList.Items))
	for i := range podList.Items {
		p := &podList.Items[i]
		eligible, reason := PodShouldEvict(p)
		out = append(out, PodDecision{Pod: p, Eligible: eligible, Reason: reason})
	}
	return out, nil
}
