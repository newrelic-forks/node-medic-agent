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
	"testing"
)

func TestStubAgent_Diagnose(t *testing.T) {
	t.Parallel()
	s := &StubAgent{}

	req := &DiagnoseRequest{
		CaseId:   "stub-test-1",
		NodeName: "node-a",
	}
	resp, attErr := s.Diagnose(context.Background(), req)
	if attErr != nil {
		t.Fatalf("StubAgent.Diagnose unexpectedly errored: %v", attErr)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.CaseId != "stub-test-1" {
		t.Errorf("CaseId = %q, want stub-test-1", resp.CaseId)
	}
	if resp.Status != "queued" {
		t.Errorf("Status = %q, want queued", resp.Status)
	}

	if got := s.Calls(); got != 1 {
		t.Errorf("Calls() = %d, want 1", got)
	}

	last := s.LastRequest()
	if last == nil || last.CaseId != "stub-test-1" {
		t.Errorf("LastRequest = %+v", last)
	}
}

func TestStubAgent_Counts(t *testing.T) {
	t.Parallel()
	s := &StubAgent{}
	for i := 0; i < 5; i++ {
		_, _ = s.Diagnose(context.Background(), &DiagnoseRequest{CaseId: "x"})
	}
	if got := s.Calls(); got != 5 {
		t.Errorf("Calls() = %d, want 5", got)
	}
}
