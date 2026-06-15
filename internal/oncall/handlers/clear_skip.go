package handlers

import (
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/audit"
	"k8s.io/node-problem-detector/internal/oncall/render"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// ClearSkipDeletion is FR-15: remove the
// machine-lifecycle.newrelic.com/skipDeletion annotation from the
// node referenced by the NHD.
//
// Idempotency mirrors uncordon — already-absent annotation returns
// 200 with {result: "already cleared (no change)"}, no audit entry
// is written, but the FR-18 stdout log line still fires.
//
// Reclaimed-node branch (FR-17a) is identical to uncordon's: 200 +
// reclaimed-shape audit entry.
func ClearSkipDeletion(deps ActionDeps) http.HandlerFunc {
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
			handleNHDFetchErr(w, r, deps.Logger, started, "clear-skip-deletion", nhdName, "", err)
			return
		}

		nodeName := nhd.Spec.Case.NodeName
		node, err := fetchNode(ctx, deps.Client, nodeName)

		if err != nil && nodeReclaimed(err) {
			result := "node no longer exists (already reclaimed)"
			if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
				Ts:     now(),
				Action: "clear-skip-deletion",
				Actor:  ActorDemoAnonymous,
				Result: "reclaimed",
				Detail: result,
			}); patchErr != nil {
				deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "clear-skip-deletion", "error", patchErr.Error())
			}
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "reclaimed", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusOK, map[string]string{"result": result})
			return
		}
		if err != nil {
			handleNodeFetchErr(w, r, deps.Logger, started, "clear-skip-deletion", nhdName, nodeName, err)
			return
		}

		if validateErr := validateNodeMatchesCase(nhd, node); validateErr != nil {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "node-mismatch",
				"message": validateErr.Error(),
			})
			return
		}

		// Idempotent path — annotation absent.
		if node.Annotations == nil {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "no-change", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusOK, map[string]string{"result": "already cleared (no change)"})
			return
		}
		if _, hasSkip := node.Annotations[render.MLCSkipDeletionAnnotation]; !hasSkip {
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "no-change", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusOK, map[string]string{"result": "already cleared (no change)"})
			return
		}

		// Patch — set the annotation key to nil so the apiserver
		// removes it (research R-6: JSON merge patch shape).
		original := node.DeepCopy()
		delete(node.Annotations, render.MLCSkipDeletionAnnotation)
		if patchErr := deps.Client.Patch(ctx, node, client.MergeFrom(original)); patchErr != nil {
			deps.Logger.Log("apiserver_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "clear-skip-deletion", "node", nodeName, "error", patchErr.Error())
			audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "error", time.Since(started).Milliseconds())
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":   "apiserver-unavailable",
				"message": patchErr.Error(),
			})
			return
		}

		if patchErr := writeAuditEntry(ctx, deps.Client, deps.AuditBufferSize, nhd, audit.UIActionEntry{
			Ts:     now(),
			Action: "clear-skip-deletion",
			Actor:  ActorDemoAnonymous,
			Result: "ok",
		}); patchErr != nil {
			deps.Logger.Log("audit_patch_failed", "request_id", server.RequestIDFromContext(ctx), "action", "clear-skip-deletion", "error", patchErr.Error())
		}

		audit.LogActionLineWith(deps.Logger, server.RequestIDFromContext(ctx), "clear-skip-deletion", nhdName, nodeName, ActorDemoAnonymous, "ok", time.Since(started).Milliseconds())
		writeJSON(w, http.StatusOK, map[string]string{"result": "ok"})
	}
}
