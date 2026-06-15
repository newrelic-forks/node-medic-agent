/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package notifier

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

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// goldenAppliedInput is the canonical Applied-message input used in
// the spec/quickstart demos. Keeping it as an exported test fixture
// lets US2 / US3 tests reuse the diagnosis shape with only the
// `kind` builder swapped.
func goldenAppliedInput() AppliedInput {
	conf := 0.85
	return AppliedInput{
		NodeName:    "ip-10-1-2-3.us-east-2.compute.internal",
		ClusterName: "test-odd-wire",
		Namespace:   "cf-monitoring",
		NHDName:     "ip-10-1-2-3-applied-fixture",
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RootCause:   "conntrack table exhausted on the node",
			RCACategory: nodemedicv1alpha1.RCACategory("Conntrack"),
			Confidence:  conf,
			AuditLogRef: &nodemedicv1alpha1.AuditLogRef{
				ObjectStore: "s3://nodemedic-audit/dev/case.jsonl",
			},
		},
	}
}

func TestBuildApplied_Shape(t *testing.T) {
	t.Parallel()
	out, err := BuildApplied(goldenAppliedInput())
	if err != nil {
		t.Fatalf("BuildApplied: %v", err)
	}

	var env slackEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	wantHeader := "NodeMedic: ip-10-1-2-3.us-east-2.compute.internal – Conntrack (conf 0.85)"
	if env.Text != wantHeader {
		t.Errorf("Text = %q, want %q", env.Text, wantHeader)
	}
	if len(env.Blocks) < 4 {
		t.Fatalf("blocks = %d, want >= 4 (header, fields, rca, kubectl)", len(env.Blocks))
	}
	if env.Blocks[0].Type != "header" || env.Blocks[0].Text == nil || env.Blocks[0].Text.Text != wantHeader {
		t.Errorf("header block wrong: %+v", env.Blocks[0])
	}
	// Fields block — assert cluster + action.
	fb := env.Blocks[1]
	if fb.Type != "section" || len(fb.Fields) != 2 {
		t.Fatalf("fields block wrong: %+v", fb)
	}
	if !strings.Contains(fb.Fields[0].Text, "test-odd-wire") {
		t.Errorf("field[0] missing cluster: %q", fb.Fields[0].Text)
	}
	if !strings.Contains(fb.Fields[1].Text, "cordoned") {
		t.Errorf("field[1] missing 'cordoned': %q", fb.Fields[1].Text)
	}
	// Body should embed the kubectl block (research R-6).
	rendered := string(out)
	if !strings.Contains(rendered, "kubectl --context=test-odd-wire -n cf-monitoring get nhd ip-10-1-2-3-applied-fixture -o yaml") {
		t.Errorf("missing kubectl block in: %s", rendered)
	}
	// Audit log reference present.
	if !strings.Contains(rendered, "s3://nodemedic-audit/dev/case.jsonl") {
		t.Errorf("missing audit log ref in: %s", rendered)
	}
}

func TestBuildApplied_NoAuditURLOmitsBlock(t *testing.T) {
	t.Parallel()
	in := goldenAppliedInput()
	in.Diagnosis.AuditLogRef = nil

	out, err := BuildApplied(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Audit log") {
		t.Errorf("expected no Audit log block when AuditLogRef is nil; got: %s", out)
	}
}

func TestBuildApplied_NilDiagnosis(t *testing.T) {
	t.Parallel()
	_, err := BuildApplied(AppliedInput{NodeName: "x"})
	if err == nil {
		t.Error("expected error for nil diagnosis")
	}
}

func goldenHumanInLoopInput() HumanInLoopInput {
	return HumanInLoopInput{
		NodeName:    "ip-10-1-2-3.us-east-2.compute.internal",
		ClusterName: "cf1z",
		Namespace:   "cf-monitoring",
		NHDName:     "ip-10-1-2-3-humaninloop-fixture",
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RootCause:   "conntrack table climbing but a single nrql data source isn't enough to commit to cordon",
			RCACategory: nodemedicv1alpha1.RCACategory("Conntrack"),
			Confidence:  0.5,
		},
		GateReason: "gate failed: confidence 0.50 < min 0.70; distinct sources 1 < min 2",
	}
}

func TestBuildHumanInLoop_Shape(t *testing.T) {
	t.Parallel()
	out, err := BuildHumanInLoop(goldenHumanInLoopInput())
	if err != nil {
		t.Fatalf("BuildHumanInLoop: %v", err)
	}

	var env slackEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	wantHeader := "NodeMedic: ip-10-1-2-3.us-east-2.compute.internal – needs human review (conf 0.50)"
	if env.Text != wantHeader {
		t.Errorf("Text = %q, want %q", env.Text, wantHeader)
	}
	if len(env.Blocks) < 5 {
		t.Fatalf("blocks = %d, want >= 5 (header, fields, gate-reason, rca, kubectl)", len(env.Blocks))
	}
	if env.Blocks[0].Text == nil || env.Blocks[0].Text.Text != wantHeader {
		t.Errorf("header block wrong: %+v", env.Blocks[0])
	}

	// Walk the decoded blocks rather than substring-matching the raw
	// JSON — keeps assertions readable and avoids tripping on JSON
	// escapes (e.g. `<` serializing to `<`).
	var (
		sawHumanReview bool
		sawNotCordoned bool
		sawGateReason  bool
		sawKubectl     bool
	)
	for _, b := range env.Blocks {
		if b.Text != nil {
			if strings.Contains(b.Text.Text, "needs human review") {
				sawHumanReview = true
			}
			if strings.Contains(b.Text.Text, "confidence 0.50 < min 0.70") {
				sawGateReason = true
			}
			if strings.Contains(b.Text.Text, "kubectl --context=cf1z -n cf-monitoring get nhd ip-10-1-2-3-humaninloop-fixture -o yaml") {
				sawKubectl = true
			}
		}
		for _, f := range b.Fields {
			if strings.Contains(f.Text, "NOT cordoned") {
				sawNotCordoned = true
			}
		}
	}
	if !sawHumanReview {
		t.Errorf("missing 'needs human review' framing in rendered blocks")
	}
	if !sawNotCordoned {
		t.Errorf("missing 'NOT cordoned' action framing in fields")
	}
	if !sawGateReason {
		t.Errorf("gate reason not surfaced verbatim: %s", out)
	}
	if !sawKubectl {
		t.Errorf("missing kubectl block: %s", out)
	}
}

func TestBuildHumanInLoop_NilDiagnosis(t *testing.T) {
	t.Parallel()
	_, err := BuildHumanInLoop(HumanInLoopInput{NodeName: "x"})
	if err == nil {
		t.Error("expected error for nil diagnosis")
	}
}

func TestBuildHumanInLoop_FallbackGateReason(t *testing.T) {
	t.Parallel()
	in := goldenHumanInLoopInput()
	in.GateReason = ""
	out, err := BuildHumanInLoop(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "no gate reason recorded") {
		t.Errorf("expected fallback text when GateReason is empty: %s", out)
	}
}

func goldenCriticalInput() CriticalInput {
	return CriticalInput{
		NodeName:      "ip-10-1-2-3.us-east-2.compute.internal",
		ClusterName:   "cf1z",
		Namespace:     "cf-monitoring",
		NHDName:       "ip-10-1-2-3-critical-fixture",
		FailureReason: "DeadlineExceeded",
		FailureDetail: "agent did not write phase=Diagnosed before retried deadline 2026-06-12T15:01:00Z",
		Attempts:      2,
	}
}

func TestBuildCritical_Shape(t *testing.T) {
	t.Parallel()
	out, err := BuildCritical(goldenCriticalInput())
	if err != nil {
		t.Fatalf("BuildCritical: %v", err)
	}

	var env slackEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	wantHeader := "NodeMedic: ip-10-1-2-3.us-east-2.compute.internal – AGENT FAILED"
	if env.Text != wantHeader {
		t.Errorf("Text = %q, want %q", env.Text, wantHeader)
	}
	if env.Blocks[0].Text == nil || env.Blocks[0].Text.Text != wantHeader {
		t.Errorf("header block wrong: %+v", env.Blocks[0])
	}

	var (
		sawAgentFailed     bool
		sawNotCordoned     bool
		sawDeadlineReason  bool
		sawDetail          bool
		sawAttemptsBurnt   bool
		sawKubectl         bool
	)
	for _, b := range env.Blocks {
		if b.Text != nil {
			t := b.Text.Text
			if strings.Contains(t, "AGENT FAILED") {
				sawAgentFailed = true
			}
			if strings.Contains(t, "DeadlineExceeded") {
				sawDeadlineReason = true
			}
			if strings.Contains(t, "agent did not write phase=Diagnosed before retried deadline") {
				sawDetail = true
			}
			if strings.Contains(t, "after 2 attempt(s)") {
				sawAttemptsBurnt = true
			}
			if strings.Contains(t, "kubectl --context=cf1z -n cf-monitoring get nhd ip-10-1-2-3-critical-fixture -o yaml") {
				sawKubectl = true
			}
		}
		for _, f := range b.Fields {
			if strings.Contains(f.Text, "NOT cordoned") {
				sawNotCordoned = true
			}
		}
	}
	if !sawAgentFailed {
		t.Errorf("missing 'AGENT FAILED' framing")
	}
	if !sawNotCordoned {
		t.Errorf("missing 'NOT cordoned' action framing")
	}
	if !sawDeadlineReason {
		t.Errorf("missing failure reason DeadlineExceeded")
	}
	if !sawDetail {
		t.Errorf("missing failure detail verbatim: %s", out)
	}
	if !sawAttemptsBurnt {
		t.Errorf("missing 'after 2 attempt(s)' framing")
	}
	if !sawKubectl {
		t.Errorf("missing kubectl block")
	}
}

func TestBuildCritical_RequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   CriticalInput
	}{
		{"empty NodeName", CriticalInput{FailureReason: "x"}},
		{"empty FailureReason", CriticalInput{NodeName: "x"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildCritical(tc.in); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestBuildCritical_DefaultAttempts(t *testing.T) {
	t.Parallel()
	in := goldenCriticalInput()
	in.Attempts = 0
	out, err := BuildCritical(in)
	if err != nil {
		t.Fatal(err)
	}
	// Default to 1 when caller didn't fill it in.
	if !strings.Contains(string(out), "after 1 attempt(s)") {
		t.Errorf("expected default 1 attempt: %s", out)
	}
}

func TestSlack_Post_Success(t *testing.T) {
	t.Parallel()
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "NodeMedic") {
			t.Errorf("body missing NodeMedic header: %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewSlack(ts.URL)
	res := s.Post(context.Background(), []byte(`{"text":"NodeMedic: hi"}`))

	if !res.Posted || res.LastErr != nil {
		t.Errorf("expected success; got %+v", res)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestSlack_Post_RetriesOn5xx(t *testing.T) {
	t.Parallel()
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewSlack(ts.URL)
	s.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	s.Sleep = func(time.Duration) {}

	res := s.Post(context.Background(), []byte(`{"text":"x"}`))
	if !res.Posted {
		t.Errorf("expected eventual success; got %+v", res)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if res.Attempts != 3 {
		t.Errorf("res.Attempts = %d, want 3", res.Attempts)
	}
}

func TestSlack_Post_GivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	s := NewSlack(ts.URL)
	s.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	s.Sleep = func(time.Duration) {}

	res := s.Post(context.Background(), []byte(`{"text":"x"}`))
	if res.Posted {
		t.Errorf("expected no success; got %+v", res)
	}
	if res.Attempts != 4 {
		t.Errorf("Attempts = %d, want 4 (1 initial + 3 retries)", res.Attempts)
	}
}

func TestSlack_Post_NoRetryOn4xx(t *testing.T) {
	t.Parallel()
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	s := NewSlack(ts.URL)
	s.Backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	s.Sleep = func(time.Duration) {}

	res := s.Post(context.Background(), []byte(`{"text":"x"}`))
	if res.Posted {
		t.Error("expected no success on 400")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (4xx not retryable)", res.Attempts)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("server saw %d POSTs, want 1", got)
	}
}

func TestSlack_Post_EmptyWebhookFailsFast(t *testing.T) {
	t.Parallel()
	s := NewSlack("")
	res := s.Post(context.Background(), []byte(`{}`))
	if res.Posted {
		t.Error("expected no success with empty webhook")
	}
	if res.LastErr == nil {
		t.Error("expected error for empty webhook")
	}
}
