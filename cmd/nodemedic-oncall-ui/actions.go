package main

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/node-problem-detector/internal/oncall/server"
)

// attachActions wires the Phase 5 + Phase 6 action handlers to the
// server. Lifted out of main() so the server.New + render-static
// wiring stays readable. Phase 4 ships this as a no-op; Phase 5
// replaces the body and Phase 6 extends it for drain.
func attachActions(srv *server.Server, kc client.Client, cfg server.Config) {
	_ = srv
	_ = kc
	_ = cfg
}
