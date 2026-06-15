package handlers

import (
	"context"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/audit"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// ActionDeps bundles the inputs the simple action handlers need.
// Phase 5 (US3) wires uncordon + clear-skip-deletion against this
// dependency set.
type ActionDeps struct {
	Client          client.Client
	Namespace       string
	Logger          *server.Logger
	AuditBufferSize int // 0 → audit.DefaultBufferSize
	Now             func() time.Time
}

// Uncordon is FR-14: patch node.spec.unschedulable=false.
//
// Idempotency: already-uncordoned nodes return 200 with
// {result: "already uncordoned (no change)"} and DO NOT write an
// audit-annotation entry (the UI MUST be quiet about no-ops). The
// FR-18 stdout log line still fires — an attempt happened.
//
// Reclaimed-node branch (FR-17a): NotFound on the Node returns 200
// with {result: "node no longer exists (already reclaimed)"} AND
// writes a single audit entry capturing the engineer's intent.
func Uncordon(deps ActionDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		started := now()
		nhdName := r.PathValue("nhd")
		ctx := r.Context()

		nhd, err := fetchNHD(ctx, deps.Client, deps.Namespace, nhdName)
		if err != nil {
			handleNHDFetchErr(w, r, deps.Logger, started, "uncordon", nhdName, "", err)
			return
		}

		nodeName := nhd.Spec.Case.NodeName
		node, err := fetchNode(ctx, deps.Client, nodeName)

		// FR-17a — reclaimed branch.
		if err != nil && nodeReclaimed(err) {
			result := "node no longer exists (already reclaimed)"
			if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
				Ts:     now(),
				Action: "uncordon",
				Actor:  ActorDemoAnonymous,
				Result: "reclaimed",
				Detail: result,
			}); patchErr != nil {
				deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "uncordon", "error", patchErr.Error())
			}
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "uncordon", nhdName, nodeName, ActorDemoAnonymous, "reclaimed", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusOK, map[string]string{"result": result})
			return
		}
		if err != nil {
			handleNodeFetchErr(w, r, deps.Logger, started, "uncordon", nhdName, nodeName, err)
			return
		}

		if validateErr := validateNodeMatchesCase(nhd, node); validateErr != nil {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "uncordon", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "node-mismatch",
				"message": validateErr.Error(),
			})
			return
		}

		// FR-14 idempotent path — already uncordoned.
		if !node.Spec.Unschedulable {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "uncordon", nhdName, nodeName, ActorDemoAnonymous, "no-change", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusOK, map[string]string{"result": "already uncordoned (no change)"})
			return
		}

		// Patch spec.unschedulable=false. Use MergeFrom against a
		// snapshot so the patch body carries only the diff.
		original := node.DeepCopy()
		node.Spec.Unschedulable = false
		if patchErr := deps.Client.Patch(ctx, node, client.MergeFrom(original)); patchErr != nil {
			deps.Logger.Log("apiserver_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "uncordon", "node", nodeName, "error", patchErr.Error())
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "uncordon", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":   "apiserver-unavailable",
				"message": patchErr.Error(),
			})
			return
		}

		// Append audit entry on the NHD.
		if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
			Ts:     now(),
			Action: "uncordon",
			Actor:  ActorDemoAnonymous,
			Result: "ok",
		}); patchErr != nil {
			deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "uncordon", "error", patchErr.Error())
		}

		audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "uncordon", nhdName, nodeName, ActorDemoAnonymous, "ok", time.Since(started).Milliseconds())
		writeJSON(w, http.StatusOK, map[string]string{"result": "ok"})
	}
}

// handleNHDFetchErr surfaces NHD fetch failures: NotFound → 404,
// other → 500 + banner. Caller hasn't done any side-effecting work
// yet, so a hard 5xx is the right shape.
func handleNHDFetchErr(w http.ResponseWriter, r *http.Request, logger *server.Logger, started time.Time, action, nhdName, nodeName string, err error) {
	if apierrors.IsNotFound(err) {
		audit.LogActionLineWith(logger, server.RequestIDFromContext(r.Context()), action, nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "nhd-not-found",
			"message": "NHD does not exist or is in a different namespace.",
		})
		return
	}
	logger.Log("apiserver_get_nhd_failed", "request_id", server.RequestIDFromContext(r.Context()), "action", action, "error", err.Error())
	audit.LogActionLineWith(logger, server.RequestIDFromContext(r.Context()), action, nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error":   "apiserver-unavailable",
		"message": err.Error(),
	})
}

// handleNodeFetchErr surfaces a non-NotFound Node fetch failure.
// NotFound is handled by the FR-17a reclaimed branch in the caller.
func handleNodeFetchErr(w http.ResponseWriter, r *http.Request, logger *server.Logger, started time.Time, action, nhdName, nodeName string, err error) {
	logger.Log("apiserver_get_node_failed", "request_id", server.RequestIDFromContext(r.Context()), "action", action, "node", nodeName, "error", err.Error())
	audit.LogActionLineWith(logger, server.RequestIDFromContext(r.Context()), action, nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error":   "apiserver-unavailable",
		"message": err.Error(),
	})
}

// writeAuditEntry encodes a new ui-action-history annotation value
// (existing+new, capped to bufSize) and JSON-merge-patches it onto
// the NHD. Bound by data-model.md §3 + research R-6.
func writeAuditEntry(ctx context.Context, c client.Client, bufSize int, nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI, entry audit.UIActionEntry) error {
	original := nhd.DeepCopy()
	raw := ""
	if anns := nhd.GetAnnotations(); anns != nil {
		raw = anns[audit.AnnotationKey]
	}
	updated, err := audit.AppendEntry(raw, entry, bufSize)
	if err != nil {
		return err
	}
	annotations := nhd.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[audit.AnnotationKey] = updated
	nhd.SetAnnotations(annotations)
	if patchErr := c.Patch(ctx, nhd, client.MergeFrom(original)); patchErr != nil {
		return patchErr
	}
	return nil
}
