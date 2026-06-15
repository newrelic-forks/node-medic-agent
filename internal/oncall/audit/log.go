package audit

import (
	"context"

	"k8s.io/node-problem-detector/internal/oncall/server"
)

// LogActionLine is the structured stdout line every action handler
// emits per FR-18. The shape:
//
//	{
//	  "ts":         "<RFC3339 UTC>",
//	  "level":      "info",
//	  "event":      "ui_action",
//	  "action":     "uncordon" | "drain" | "clear-skip-deletion",
//	  "nhd_name":   "...",
//	  "node":       "...",
//	  "actor":      "demo-anonymous",
//	  "result":     "ok" | "reclaimed" | "error" | "partial",
//	  "duration_ms": <int>,
//	  "request_id": "<X-Request-ID>"
//	}
//
// Caller passes a context carrying the per-request logger (so a
// request-id field surfaces) and the resolved fields. Idempotent
// "no-change" paths emit the line too — an action was attempted,
// and AC-13 wants every attempt visible — but do NOT append to the
// audit annotation.
func LogActionLine(ctx context.Context, action, nhdName, node, actor, result string, durationMs int64) {
	logger := server.LoggerFromContext(ctx)
	logger.Log("ui_action",
		"action", action,
		"nhd_name", nhdName,
		"node", node,
		"actor", actor,
		"result", result,
		"duration_ms", durationMs,
		"request_id", server.RequestIDFromContext(ctx),
	)
}

// LogActionLineWith is the explicit-logger variant for handlers that
// don't carry the logger via context.
func LogActionLineWith(logger *server.Logger, requestID, action, nhdName, node, actor, result string, durationMs int64) {
	logger.Log("ui_action",
		"action", action,
		"nhd_name", nhdName,
		"node", node,
		"actor", actor,
		"result", result,
		"duration_ms", durationMs,
		"request_id", requestID,
	)
}
