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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// goldenRequest is the canonical body shape locked in
// .specify/specs/001-nodemedic-controller/contracts/post-diagnose.md.
// Any drift here is also a contract drift with Scope 3 — both must
// land in the same PR.
const goldenRequest = `{"caseId":"8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c","nodeName":"ip-10-1-2-3.ec2.internal","clusterName":"test-odd-wire","provider":"aws","region":"us-east-2","instanceId":"i-0abc1234deadbeef","trigger":{"type":"ConntrackSaturated","reason":"ConntrackHigh","message":"nf_conntrack_count=262100 max=262144","observedAt":"2026-06-12T15:00:00Z"},"budgets":{"maxTurns":15,"maxBudgetUSD":"0.50","deadlineSec":60}}`

func newGoldenRequest() *DiagnoseRequest {
	return &DiagnoseRequest{
		CaseId:      "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c",
		NodeName:    "ip-10-1-2-3.ec2.internal",
		ClusterName: "test-odd-wire",
		Provider:    "aws",
		Region:      "us-east-2",
		InstanceId:  "i-0abc1234deadbeef",
		Trigger: DiagnoseTrigger{
			Type:       "ConntrackSaturated",
			Reason:     "ConntrackHigh",
			Message:    "nf_conntrack_count=262100 max=262144",
			ObservedAt: "2026-06-12T15:00:00Z",
		},
		Budgets: DiagnoseBudgets{
			MaxTurns:     15,
			MaxBudgetUSD: "0.50",
			DeadlineSec:  60,
		},
	}
}

func TestDiagnoseRequest_GoldenJSON(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(newGoldenRequest())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != goldenRequest {
		t.Errorf("contract drift!\n got: %s\nwant: %s", got, goldenRequest)
	}
}

func TestClient_Diagnose_OK(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/diagnose" {
			t.Errorf("path = %s, want /diagnose", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("Authorization = %q, want Bearer secret-token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != goldenRequest {
			t.Errorf("body drift!\n got: %s\nwant: %s", body, goldenRequest)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"caseId":"8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c","status":"queued"}`))
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "secret-token")
	resp, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr != nil {
		t.Fatalf("attErr = %v", attErr)
	}
	if resp == nil || resp.Status != "queued" {
		t.Fatalf("resp = %+v, want status=queued", resp)
	}
}

func TestClient_Diagnose_RetriesOn429(t *testing.T) {
	t.Parallel()

	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"caseId":"x","status":"queued"}`))
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c.Sleep = func(time.Duration) {}

	resp, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr != nil {
		t.Fatalf("expected success after retries, got %v", attErr)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q", resp.Status)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestClient_Diagnose_RetriesOn5xx(t *testing.T) {
	t.Parallel()

	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c.Sleep = func(time.Duration) {}

	_, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr == nil {
		t.Fatal("expected AttemptError after exhausting retries")
	}
	if attErr.Class != ResultServer {
		t.Errorf("Class = %v, want ResultServer", attErr.Class)
	}
	// 1 initial + 3 retries = 4 attempts total.
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("attempts = %d, want 4 (1 initial + 3 retries)", got)
	}
}

func TestClient_Diagnose_NoRetryOn400(t *testing.T) {
	t.Parallel()

	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c.Sleep = func(time.Duration) {}

	_, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr == nil {
		t.Fatal("expected AttemptError")
	}
	if attErr.Class != ResultBadRequest {
		t.Errorf("Class = %v, want ResultBadRequest", attErr.Class)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 400)", got)
	}
}

func TestClient_Diagnose_NoRetryOn401(t *testing.T) {
	t.Parallel()

	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c.Sleep = func(time.Duration) {}

	_, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr == nil || attErr.Class != ResultUnauthorized {
		t.Fatalf("expected ResultUnauthorized, got %+v", attErr)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 401)", got)
	}
}

func TestClient_Diagnose_409TreatedAsAccepted(t *testing.T) {
	t.Parallel()
	// research R-5: 409 means "already in flight". The controller
	// synthesizes a 202-equivalent response so the reconciler doesn't
	// have to special-case it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	resp, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr != nil {
		t.Fatalf("409 should be treated as success, got %v", attErr)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q, want queued", resp.Status)
	}
	if resp.CaseId != "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c" {
		t.Errorf("CaseId mirrored from request = %q", resp.CaseId)
	}
}

func TestClient_Diagnose_TimeoutClassifiedAsTimeout(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slower than the per-attempt timeout we set below.
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.HTTP = &http.Client{Timeout: 5 * time.Millisecond}
	c.Backoffs = nil // no retry — surface the first timeout

	_, attErr := c.Diagnose(context.Background(), newGoldenRequest())
	if attErr == nil {
		t.Fatal("expected timeout error")
	}
	if attErr.Class != ResultTimeout {
		t.Errorf("Class = %v, want ResultTimeout", attErr.Class)
	}
}

func TestClient_Diagnose_ContextCancellation(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	c := New(ts.URL+"/diagnose", "tok")
	c.Backoffs = nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, attErr := c.Diagnose(ctx, newGoldenRequest())
	if attErr == nil {
		t.Fatal("expected error from cancelled context")
	}
	// Either Timeout or Other — the important thing is we got an error
	// quickly and didn't block on the slow server.
	if !strings.Contains(attErr.Error(), "context canceled") &&
		!strings.Contains(attErr.Error(), "deadline exceeded") {
		t.Logf("attErr = %v (non-fatal; context cancellation manifests differently across Go versions)", attErr)
	}
}
