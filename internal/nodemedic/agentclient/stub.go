/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agentclient

import (
	"context"
	"sync"
	"sync/atomic"
)

// StubAgent is a no-op AgentClient that records every Diagnose call
// and pretends the agent returned 202 `queued` without touching the
// network. Used by --stub-agent mode while Scope 3's agent service
// isn't deployed yet, so the controller can drive its phase machine
// past the Pending → Diagnosing transition without network failures.
//
// Calls are recorded so the demo flow can show "the controller did
// invoke the agent" via metrics + structured log, while the actual
// status.diagnosis is hand-applied via `kubectl apply --subresource=status`
// from a fixture.
type StubAgent struct {
	mu    sync.Mutex
	count atomic.Int64
	last  *DiagnoseRequest
}

// Diagnose records the request and returns 202 `queued`. Never errors.
func (s *StubAgent) Diagnose(_ context.Context, req *DiagnoseRequest) (*DiagnoseResponse, *AttemptError) {
	s.count.Add(1)
	s.mu.Lock()
	s.last = req
	s.mu.Unlock()
	return &DiagnoseResponse{CaseId: req.CaseId, Status: "queued"}, nil
}

// Calls returns the cumulative count of Diagnose invocations across
// the lifetime of this StubAgent.
func (s *StubAgent) Calls() int64 { return s.count.Load() }

// LastRequest returns the most recent DiagnoseRequest, or nil if
// Diagnose has never been called. Returns a defensive copy so the
// caller can't mutate StubAgent's internal state.
func (s *StubAgent) LastRequest() *DiagnoseRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil
	}
	cp := *s.last
	return &cp
}
