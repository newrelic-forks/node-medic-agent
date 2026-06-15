package handlers

import (
	"context"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/render"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// DetailDeps bundles the inputs the detail handler needs.
type DetailDeps struct {
	Client    client.Client
	Namespace string
	Logger    *server.Logger
}

// DetailHTML returns a handler for `GET /cases/{nhd}` that renders the
// per-case page. FR-9/FR-9a/FR-10/FR-11/FR-12/FR-13 are bound here.
//
//   - NHD NotFound → 200 with "Case not found" panel (FR-12).
//   - Apiserver unreachable → 200 with FR-13 inline error banner.
//   - Node NotFound → reclaimed-banner branch via composeDetailPageData.
func DetailHTML(deps DetailDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nhdName := r.PathValue("nhd")
		render.WriteHTMLHeader(w.Header())

		nhd, errNHD := getNHD(r.Context(), deps.Client, deps.Namespace, nhdName)
		if errNHD != nil {
			if apierrors.IsNotFound(errNHD) {
				w.WriteHeader(http.StatusOK)
				if rerr := render.RenderDetail(w, render.DetailPageData{NHDName: nhdName, Namespace: deps.Namespace}, "", true); rerr != nil {
					deps.Logger.Log("template_render_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", rerr.Error())
				}
				return
			}
			deps.Logger.Log("apiserver_get_nhd_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", errNHD.Error())
			w.WriteHeader(http.StatusOK)
			if rerr := render.RenderDetail(w, render.DetailPageData{NHDName: nhdName, Namespace: deps.Namespace}, errNHD.Error(), false); rerr != nil {
				deps.Logger.Log("template_render_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", rerr.Error())
			}
			return
		}

		node, nodeNotFound, errNode := getNode(r.Context(), deps.Client, nhd.Spec.Case.NodeName)
		errBanner := ""
		if errNode != nil {
			errBanner = errNode.Error()
			deps.Logger.Log("apiserver_get_node_failed", "request_id", server.RequestIDFromContext(r.Context()), "node", nhd.Spec.Case.NodeName, "error", errNode.Error())
		}

		page := render.ComposeDetailPageData(nhd, node, nodeNotFound)
		w.WriteHeader(http.StatusOK)
		if rerr := render.RenderDetail(w, page, errBanner, false); rerr != nil {
			deps.Logger.Log("template_render_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", rerr.Error())
		}
	}
}

// getNHD round-trips the apiserver for one NHD. FR-24 — no informer.
func getNHD(ctx context.Context, c client.Client, namespace, name string) (*nodemedicv1alpha1.NodeHealthDiagnosisAI, error) {
	var nhd nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &nhd); err != nil {
		return nil, err
	}
	return &nhd, nil
}

// getNode round-trips the apiserver for one Node, distinguishing
// NotFound (the FR-17a benign-reclaimed branch) from other errors.
func getNode(ctx context.Context, c client.Client, name string) (*corev1.Node, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return &node, false, nil
}
