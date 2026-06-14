/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/nodemedic/notifier"
)

// T026a — locks the call-site switch added in T029.
//
// Each subtest drives one of the three terminal paths (applyPath /
// humanInLoopPath / criticalFailure) under both useBlockKit=true and
// useBlockKit=false, captures the bytes posted to Slack, and asserts:
//   - useBlockKit=true → matches the BuildBlockKit* golden tree
//   - useBlockKit=false → matches the existing plain-text shape (no
//     "blocks" key — BuildApplied/BuildHumanInLoop/BuildCritical use
//     `blocks: [...]` so we assert the SHAPE differs from the Block
//     Kit golden, not byte-equality with the legacy builders)
//
// Tests assume the production reconciler has fields:
//   - UseBlockKit bool
//   - UIBaseURL   string
// and that applyPath/humanInLoopPath/criticalFailure read them at the
// builder selection site. T029 adds those fields.

// recordingSlack captures the last payload posted; never errors.
type recordingSlack struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (s *recordingSlack) Post(_ context.Context, payload []byte) notifier.PostResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	s.payloads = append(s.payloads, cp)
	return notifier.PostResult{Posted: true, Attempts: 1}
}

func (s *recordingSlack) last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.payloads) == 0 {
		return nil
	}
	return s.payloads[len(s.payloads)-1]
}

// logRecord captures one Info-level log call so tests can assert the
// FR-4 `slack_post` line was emitted at the call site with the right
// fields.
type logRecord struct {
	msg    string
	fields map[string]any
}

// logSink is a thread-safe logr sink that captures every Info call.
// Wired into ctx via ctrllog.IntoContext so the reconciler's
// `log.FromContext(ctx)` picks it up — no production code changes
// needed.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

func newLogSink() *logSink { return &logSink{} }

func (s *logSink) intoCtx(ctx context.Context) context.Context {
	return ctrllog.IntoContext(ctx, logr.New(&captureSink{outer: s}))
}

func (s *logSink) all() []logRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]logRecord, len(s.records))
	copy(out, s.records)
	return out
}

// captureSink is a minimal logr.LogSink that records Info calls into
// the parent logSink. We do not need WithName/WithValues fidelity for
// these assertions — the reconciler's `slack_post` call passes all the
// fields we care about as direct kvList args.
type captureSink struct {
	outer *logSink
}

func (c *captureSink) Init(logr.RuntimeInfo)          {}
func (c *captureSink) Enabled(int) bool               { return true }
func (c *captureSink) WithName(string) logr.LogSink   { return c }
func (c *captureSink) WithValues(...any) logr.LogSink { return c }

func (c *captureSink) Info(_ int, msg string, kvList ...any) {
	rec := logRecord{msg: msg, fields: map[string]any{}}
	for i := 0; i+1 < len(kvList); i += 2 {
		key, _ := kvList[i].(string)
		rec.fields[key] = kvList[i+1]
	}
	c.outer.mu.Lock()
	defer c.outer.mu.Unlock()
	c.outer.records = append(c.outer.records, rec)
}

func (c *captureSink) Error(error, string, ...any) {}

// findSlackPost returns the (single expected) slack_post record from
// the sink, or fails the test loudly. The reconciler emits exactly one
// `slack_post` per terminal-phase Slack call.
func (s *logSink) findSlackPost(t *testing.T) logRecord {
	t.Helper()
	var hits []logRecord
	for _, r := range s.all() {
		if r.msg == "slack_post" {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 slack_post log record, got %d (records=%+v)",
			len(hits), s.all())
	}
	return hits[0]
}

// blockKitGolden parses the testdata file from notifier package so the
// reconciler test can compare its Slack payload against the same tree
// the notifier-package golden tests use.
func blockKitGolden(t *testing.T, name string) any {
	t.Helper()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// pwd is internal/nodemedic/controller; goldens live two levels up
	// in internal/nodemedic/notifier/testdata/block-kit/<name>.json.
	p := filepath.Join(pwd, "..", "notifier", "testdata", "block-kit", name+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v", p, err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("parse golden %s: %v", p, err)
	}
	return tree
}

func parsePayload(t *testing.T, b []byte) any {
	t.Helper()
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("parse payload: %v\nraw: %s", err, string(b))
	}
	return tree
}

// hasBlocksField reports whether the parsed Slack payload has a
// top-level "blocks" array. Both the legacy and Block Kit builders
// emit blocks; we use this only as a sanity guard — the SHAPE check
// (header text format) tells the formats apart.
func hasBlocksField(tree any) bool {
	m, ok := tree.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["blocks"]
	return ok
}

// headerText reads blocks[0].text.text — the first thing a human reads.
// Block Kit US1 headers begin with "🚨 Cordoned: " / "⚠️ Needs review: " /
// "❌ Failed: ". Legacy headers begin with "NodeMedic: ".
func headerText(t *testing.T, tree any) string {
	t.Helper()
	m, ok := tree.(map[string]any)
	if !ok {
		t.Fatalf("payload is not an object: %T", tree)
	}
	blocks, ok := m["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("payload missing blocks: %v", m)
	}
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(map[string]any)
	s, _ := text["text"].(string)
	return s
}

func reconcilerForBlockKitTest(t *testing.T, useBlockKit bool, objs ...client.Object) (*NHDReconciler, *recordingSlack, *logSink) {
	t.Helper()
	scheme := nhdScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&nodemedicv1alpha1.NodeHealthDiagnosisAI{}).
		Build()

	slack := &recordingSlack{}
	rec := &NHDReconciler{
		Client:             cli,
		Recorder:           record.NewFakeRecorder(100),
		Slack:              slack,
		MinConfidence:      0.7,
		MinEvidenceSources: 2,
		ClusterName:        "cf1z",
		Namespace:          "cf-monitoring",
		UseBlockKit:        useBlockKit,
		UIBaseURL:          "http://localhost:8080",
		Now: func() time.Time {
			return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
		},
	}
	return rec, slack, newLogSink()
}

// assertSlackPostLog locks the FR-4 `slack_post` record shape: it must
// be the only such record, the kind must match the call site, and
// `block_kit` must reflect the reconciler's UseBlockKit field. The
// cf1z gate (T033/T034) greps for `block_kit=true` from these lines
// to confirm the new builder took effect.
func assertSlackPostLog(t *testing.T, sink *logSink, wantKind string, wantBlockKit bool) {
	t.Helper()
	rec := sink.findSlackPost(t)
	if got := rec.fields["event"]; got != "slack_post" {
		t.Errorf("slack_post log: event field = %v, want %q", got, "slack_post")
	}
	if got := rec.fields["kind"]; got != wantKind {
		t.Errorf("slack_post log: kind = %v, want %q", got, wantKind)
	}
	if got := rec.fields["block_kit"]; got != wantBlockKit {
		t.Errorf("slack_post log: block_kit = %v, want %v", got, wantBlockKit)
	}
	if got := rec.fields["posted"]; got != true {
		t.Errorf("slack_post log: posted = %v, want true", got)
	}
}

// blockKitNHDApplied builds an NHD already in phase=Diagnosed with a
// confidence of 0.92, RCACategory=Kubelet, two distinct evidence
// sources, and a Cordon recommendation — i.e. the gate WILL pass.
// Matches the contracts/slack-block-kit.md §1 input shape.
func blockKitNHDApplied() *nodemedicv1alpha1.NodeHealthDiagnosisAI {
	return &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf1z-general-nodes-1000007-1718374200",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "11111111-1111-4111-8111-111111111111",
				NodeName:    "cf1z-general-nodes-1000007",
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAzure,
				Region:      "eastus2",
				InstanceId:  "vm-1000007",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "KubeletUnhealthy",
					ObservedAt: metav1.NewTime(time.Date(2026, 6, 14, 11, 0, 0, 0, time.UTC)),
				},
			},
		},
		Status: nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
			Phase: nodemedicv1alpha1.PhaseDiagnosed,
			Diagnosis: &nodemedicv1alpha1.Diagnosis{
				RootCause:   "kubelet not posting status",
				RCACategory: nodemedicv1alpha1.RCACategory("Kubelet"),
				Confidence:  0.92,
				Evidence: []nodemedicv1alpha1.EvidenceItem{
					{Source: nodemedicv1alpha1.EvidenceSource("kubectl")},
					{Source: nodemedicv1alpha1.EvidenceSource("nrql")},
				},
				Recommendation: &nodemedicv1alpha1.Recommendation{
					Action: nodemedicv1alpha1.RecommendationActionCordon,
				},
			},
		},
	}
}

func blockKitNHDHumanInLoop() *nodemedicv1alpha1.NodeHealthDiagnosisAI {
	return &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf1z-general-nodes-2000007-1718374500",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "22222222-2222-4222-8222-222222222222",
				NodeName:    "cf1z-general-nodes-2000007",
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAzure,
				Region:      "eastus2",
				InstanceId:  "vm-2000007",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "KubeletUnhealthy",
					ObservedAt: metav1.NewTime(time.Date(2026, 6, 14, 11, 5, 0, 0, time.UTC)),
				},
			},
		},
		Status: nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
			Phase: nodemedicv1alpha1.PhaseDiagnosed,
			Diagnosis: &nodemedicv1alpha1.Diagnosis{
				RootCause:   "low confidence — gate fails",
				RCACategory: nodemedicv1alpha1.RCACategory("Kubelet"),
				Confidence:  0.42,
				Evidence: []nodemedicv1alpha1.EvidenceItem{
					{Source: nodemedicv1alpha1.EvidenceSource("kubectl")},
				},
				Recommendation: &nodemedicv1alpha1.Recommendation{
					Action: nodemedicv1alpha1.RecommendationActionCordon,
				},
			},
		},
	}
}

func blockKitNHDFailed() *nodemedicv1alpha1.NodeHealthDiagnosisAI {
	return &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf1z-general-nodes-3000007-1718375100",
			Namespace: "cf-monitoring",
			Annotations: map[string]string{
				// retry-count=1 so criticalFailure fires (FR-7 second hit).
				RetryCountAnnotation: "1",
			},
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "33333333-3333-4333-8333-333333333333",
				NodeName:    "cf1z-general-nodes-3000007",
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAzure,
				Region:      "eastus2",
				InstanceId:  "vm-3000007",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "KubeletUnhealthy",
					ObservedAt: metav1.NewTime(time.Date(2026, 6, 14, 11, 10, 0, 0, time.UTC)),
				},
			},
		},
		Status: nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
			Phase: nodemedicv1alpha1.PhaseDiagnosing,
		},
	}
}

func TestApplyPath_UseBlockKitTrue_PostsBlockKitApplied(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDApplied()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nhd.Spec.Case.NodeName}}
	rec, slack, sink := reconcilerForBlockKitTest(t, true, nhd, node)

	gate := ConfidenceGate(nhd.Status.Diagnosis, rec.MinConfidence, rec.MinEvidenceSources)
	if gate.Outcome != GatePass {
		t.Fatalf("test fixture expected GatePass, got %s (%s)", gate.Outcome.String(), gate.Reason)
	}
	if _, err := rec.applyPath(sink.intoCtx(context.Background()), nhd, gate); err != nil {
		t.Fatalf("applyPath: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	wantTree := blockKitGolden(t, "applied")
	if diff := cmp.Diff(wantTree, parsePayload(t, got)); diff != "" {
		t.Fatalf("Block Kit Applied payload diverges from golden:\n%s", diff)
	}
	assertSlackPostLog(t, sink, "Applied", true)
}

func TestApplyPath_UseBlockKitFalse_PostsLegacyApplied(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDApplied()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nhd.Spec.Case.NodeName}}
	rec, slack, sink := reconcilerForBlockKitTest(t, false, nhd, node)

	gate := ConfidenceGate(nhd.Status.Diagnosis, rec.MinConfidence, rec.MinEvidenceSources)
	if gate.Outcome != GatePass {
		t.Fatalf("test fixture expected GatePass, got %s", gate.Outcome.String())
	}
	if _, err := rec.applyPath(sink.intoCtx(context.Background()), nhd, gate); err != nil {
		t.Fatalf("applyPath: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	tree := parsePayload(t, got)
	if !hasBlocksField(tree) {
		t.Fatal("legacy Applied payload missing blocks field")
	}
	if h := headerText(t, tree); h == "🚨 Cordoned: cf1z-general-nodes-1000007 (cf1z)" {
		t.Fatalf("legacy payload used Block Kit header %q; expected legacy NodeMedic header", h)
	}
	// Sanity: legacy header is the "NodeMedic: <node> – ..." form.
	if h := headerText(t, tree); h == "" || h[:len("NodeMedic:")] != "NodeMedic:" {
		t.Fatalf("legacy header should start with %q; got %q", "NodeMedic:", h)
	}
	assertSlackPostLog(t, sink, "Applied", false)
}

func TestHumanInLoopPath_UseBlockKitTrue_PostsBlockKitHumanInLoop(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDHumanInLoop()
	rec, slack, sink := reconcilerForBlockKitTest(t, true, nhd)

	gate := ConfidenceGate(nhd.Status.Diagnosis, rec.MinConfidence, rec.MinEvidenceSources)
	if gate.Outcome == GatePass {
		t.Fatalf("test fixture expected GateFail or GateSkip, got GatePass")
	}
	if _, err := rec.humanInLoopPath(sink.intoCtx(context.Background()), nhd, gate); err != nil {
		t.Fatalf("humanInLoopPath: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	wantTree := blockKitGolden(t, "human-in-loop")
	if diff := cmp.Diff(wantTree, parsePayload(t, got)); diff != "" {
		t.Fatalf("Block Kit HumanInLoop payload diverges from golden:\n%s", diff)
	}
	assertSlackPostLog(t, sink, "HumanInLoop", true)
}

func TestHumanInLoopPath_UseBlockKitFalse_PostsLegacyHumanInLoop(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDHumanInLoop()
	rec, slack, sink := reconcilerForBlockKitTest(t, false, nhd)

	gate := ConfidenceGate(nhd.Status.Diagnosis, rec.MinConfidence, rec.MinEvidenceSources)
	if _, err := rec.humanInLoopPath(sink.intoCtx(context.Background()), nhd, gate); err != nil {
		t.Fatalf("humanInLoopPath: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	if h := headerText(t, parsePayload(t, got)); h == "" || h[:len("NodeMedic:")] != "NodeMedic:" {
		t.Fatalf("legacy header should start with %q; got %q", "NodeMedic:", h)
	}
	assertSlackPostLog(t, sink, "HumanInLoop", false)
}

func TestCriticalFailure_UseBlockKitTrue_PostsBlockKitFailed(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDFailed()
	rec, slack, sink := reconcilerForBlockKitTest(t, true, nhd)

	if _, err := rec.criticalFailure(sink.intoCtx(context.Background()), nhd, "AgentUnreachable",
		"agent did not write phase=Diagnosed before retried deadline"); err != nil {
		t.Fatalf("criticalFailure: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	wantTree := blockKitGolden(t, "failed")
	if diff := cmp.Diff(wantTree, parsePayload(t, got)); diff != "" {
		t.Fatalf("Block Kit Failed payload diverges from golden:\n%s", diff)
	}
	assertSlackPostLog(t, sink, "Critical", true)
}

func TestCriticalFailure_UseBlockKitFalse_PostsLegacyCritical(t *testing.T) {
	t.Parallel()

	nhd := blockKitNHDFailed()
	rec, slack, sink := reconcilerForBlockKitTest(t, false, nhd)

	if _, err := rec.criticalFailure(sink.intoCtx(context.Background()), nhd, "AgentUnreachable",
		"agent did not write phase=Diagnosed before retried deadline"); err != nil {
		t.Fatalf("criticalFailure: %v", err)
	}

	got := slack.last()
	if got == nil {
		t.Fatal("no Slack payload posted")
	}
	if h := headerText(t, parsePayload(t, got)); h == "" || h[:len("NodeMedic:")] != "NodeMedic:" {
		t.Fatalf("legacy header should start with %q; got %q", "NodeMedic:", h)
	}
	assertSlackPostLog(t, sink, "Critical", false)
}
