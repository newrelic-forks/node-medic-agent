package main

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/handlers"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

// attachActions wires the Phase 5 + Phase 6 action handlers to the
// server. Lifted out of main() so the server.New + render-static
// wiring stays readable.
func attachActions(srv *server.Server, kc client.Client, cfg server.Config) {
	actDeps := handlers.ActionDeps{
		Client:          kc,
		Namespace:       cfg.Namespace,
		Logger:          srv.Logger(),
		AuditBufferSize: cfg.AuditBufferSize,
	}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/uncordon", handlers.Uncordon(actDeps))
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/clear-skip-deletion", handlers.ClearSkipDeletion(actDeps))

	drainDeps := handlers.DrainDeps{
		Client:           kc,
		Namespace:        cfg.Namespace,
		Logger:           srv.Logger(),
		DrainConcurrency: cfg.DrainConcurrency,
		AuditBufferSize:  cfg.AuditBufferSize,
	}
	srv.SetRouteHandler("POST /api/cases/{nhd}/actions/drain", handlers.Drain(drainDeps))
}
