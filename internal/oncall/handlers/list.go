// Package handlers carries the on-call UI's HTTP handler bodies.
//
// Each handler is constructed with the bare dependencies it needs
// (kube client, namespace, logger) so the server.Server type stays a
// thin assembly point. Tests construct handlers directly with
// fake.NewClientBuilder-backed clients.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/render"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// ListDeps bundles the inputs the list handlers need.
type ListDeps struct {
	Client    client.Client
	Namespace string
	Logger    *server.Logger
	Now       func() time.Time
}

// ListHTML returns a handler for `GET /` that renders the list HTML.
// FR-5/FR-6/FR-7/FR-8 are bound here. The apiserver-unavailable path
// (FR-13) emits 200 with an inline error banner, NOT a 5xx — the
// engineer always sees a page they can read.
func ListHTML(deps ListDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		rows, listErr := listRows(r.Context(), deps.Client, deps.Namespace, now())
		render.WriteHTMLHeader(w.Header())
		w.WriteHeader(http.StatusOK)
		errBanner := ""
		if listErr != nil {
			errBanner = listErr.Error()
			deps.Logger.Log("apiserver_list_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", listErr.Error())
		}
		if err := render.RenderList(w, rows, errBanner); err != nil {
			deps.Logger.Log("template_render_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", err.Error())
		}
	}
}

// ListJSON returns a handler for `GET /api/cases` that emits the
// same data as the SSR page, JSON-encoded.
func ListJSON(deps ListDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		rows, listErr := listRows(r.Context(), deps.Client, deps.Namespace, now())
		w.Header().Set("Content-Type", "application/json")
		if listErr != nil {
			deps.Logger.Log("apiserver_list_failed", "request_id", server.RequestIDFromContext(r.Context()), "error", listErr.Error())
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "apiserver-unavailable",
				"message": listErr.Error(),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		// Emit `[]` not `null` for the empty-result case so the JS
		// consumer can rely on Array.isArray.
		if rows == nil {
			rows = []render.ListPageRow{}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}
}

// listRows is the shared helper: List NHDs in the namespace, fold to
// rows, filter to the last 24h, sort newest first.
func listRows(ctx context.Context, c client.Client, namespace string, now time.Time) ([]render.ListPageRow, error) {
	var list nodemedicv1alpha1.NodeHealthDiagnosisAIList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return render.ComposeListRows(list.Items, now), nil
}
