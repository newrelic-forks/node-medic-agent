// T074 — table-driven tests for drain.PodShouldEvict (research R-3
// + spec FR-16).
package unit

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/node-problem-detector/internal/oncall/drain"
)

func TestPodShouldEvict(t *testing.T) {
	now := metav1.NewTime(time.Now())

	cases := []struct {
		name       string
		pod        *corev1.Pod
		wantEvict  bool
		wantReason drain.SkipReason
	}{
		{
			name:      "regular workload pod",
			pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
			wantEvict: true,
		},
		{
			name: "DaemonSet pod skipped",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "ds-pod",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "DaemonSet", Name: "ds"},
				},
			}},
			wantEvict:  false,
			wantReason: drain.SkipDaemonSet,
		},
		{
			name: "mirror pod skipped",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name:        "mirror",
				Annotations: map[string]string{"kubernetes.io/config.mirror": "yes"},
			}},
			wantEvict:  false,
			wantReason: drain.SkipMirror,
		},
		{
			name: "system-node-critical pod skipped",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy"},
				Spec:       corev1.PodSpec{PriorityClassName: drain.PriorityClassSystemNodeCritical},
			},
			wantEvict:  false,
			wantReason: drain.SkipSystemNodeCritical,
		},
		{
			name: "terminating pod skipped",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "going-away",
					DeletionTimestamp: &now,
				},
			},
			wantEvict:  false,
			wantReason: drain.SkipTerminating,
		},
		{
			name:      "nil pod returns false",
			pod:       nil,
			wantEvict: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := drain.PodShouldEvict(tc.pod)
			if got != tc.wantEvict {
				t.Errorf("PodShouldEvict eligible = %v, want %v", got, tc.wantEvict)
			}
			if reason != tc.wantReason {
				t.Errorf("PodShouldEvict reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
