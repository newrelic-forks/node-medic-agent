package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// ActorDemoAnonymous is the literal v1 actor identity (spec §0 Q6 +
// data-model.md §3). Production restoration adds SSO and replaces
// this constant with the authenticated user.
const ActorDemoAnonymous = "demo-anonymous"

// ErrNodeMismatch is returned by validateNodeMatchesCase when the
// NHD's spec.case.nodeName doesn't match the Node CR's name. The
// UI never produces such requests; the action endpoints surface it
// as a 400 defensive belt (FR-17).
var ErrNodeMismatch = errors.New("node-mismatch: NHD's case.nodeName does not match the targeted node")

// validateNodeMatchesCase enforces FR-17. Pure: no kube I/O — caller
// has already fetched both objects.
func validateNodeMatchesCase(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI, node *corev1.Node) error {
	if nhd == nil {
		return errors.New("validateNodeMatchesCase: nhd is nil")
	}
	if node == nil {
		return errors.New("validateNodeMatchesCase: node is nil")
	}
	if nhd.Spec.Case.NodeName == "" {
		return errors.New("validateNodeMatchesCase: nhd.spec.case.nodeName is empty")
	}
	if nhd.Spec.Case.NodeName != node.Name {
		return ErrNodeMismatch
	}
	return nil
}

// nodeReclaimed reports whether err signals the FR-17a benign
// "node not found" branch.
func nodeReclaimed(err error) bool {
	return apierrors.IsNotFound(err)
}

// fetchNHD GETs the named NHD from the apiserver. Returns nil + err
// on any failure (handler decides whether 404 vs 500 vs banner).
func fetchNHD(ctx context.Context, c client.Client, namespace, name string) (*nodemedicv1alpha1.NodeHealthDiagnosisAI, error) {
	var nhd nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &nhd); err != nil {
		return nil, err
	}
	return &nhd, nil
}

// fetchNode GETs the node by name. Returns (node, nil) on success,
// (nil, IsNotFound err) if the node was reclaimed, (nil, other err)
// on any other apiserver failure.
func fetchNode(ctx context.Context, c client.Client, name string) (*corev1.Node, error) {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// writeJSON writes obj as JSON with the named status code. Used by
// the action handlers — Cache-Control: no-store is set by the
// server's middleware.
func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}
