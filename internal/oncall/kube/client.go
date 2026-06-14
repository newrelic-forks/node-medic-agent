// Package kube builds the on-call UI's controller-runtime client.
//
// The client returned by BuildClient is a direct typed client.Client with
// no informer cache: every list, get, and patch round-trips the
// apiserver. Spec FR-24 binds this — "MUST NOT cache NHD reads beyond a
// single request" — to avoid the watch-resync staleness window an
// informer-backed surface would introduce. The trade-off (slightly
// chattier reads, no staleness) is acknowledged in plan §Storage and
// research R-5.
package kube

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// BuildScheme registers every API type the UI reads or writes:
//   - core/v1 (Node, Pod)
//   - policy/v1 (Eviction subresource)
//   - nodemedic.cf.newrelic.com/v1alpha1 (NodeHealthDiagnosisAI)
//
// Scheme registration is the only piece of kube wiring that needs to
// stay in sync with the controller and agent — every other surface is
// independent.
func BuildScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register corev1: %w", err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register policyv1: %w", err)
	}
	if err := nodemedicv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register nodemedic v1alpha1: %w", err)
	}
	return scheme, nil
}

// BuildClient returns a direct typed client.Client backed by the
// apiserver — no manager, no informer, no cache. Every Get/List/Patch
// round-trips the apiserver per FR-24.
//
// Resolves rest.Config from the in-cluster ServiceAccount mount
// (/var/run/secrets/kubernetes.io/serviceaccount/) by default; respects
// $KUBECONFIG / $HOME/.kube/config when running outside a cluster
// (handy for local dev — pass --cluster-name=test-* and a kubeconfig
// pointed at a sandbox).
func BuildClient() (client.Client, *rest.Config, error) {
	scheme, err := BuildScheme()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve rest config: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("build client: %w", err)
	}
	return c, cfg, nil
}
