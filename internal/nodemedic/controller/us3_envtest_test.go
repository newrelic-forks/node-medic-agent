//go:build integration

/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/nodemedic/agentclient"
	"k8s.io/node-problem-detector/internal/nodemedic/controller"
	"k8s.io/node-problem-detector/internal/nodemedic/notifier"
)

// silentAgent always returns 202 — useful for the deadline-retry path
// where the agent is "alive" but never updates the CR.
type silentAgent struct {
	mu    sync.Mutex
	calls []*agentclient.DiagnoseRequest
}

func (s *silentAgent) Diagnose(_ context.Context, req *agentclient.DiagnoseRequest) (*agentclient.DiagnoseResponse, *agentclient.AttemptError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	return &agentclient.DiagnoseResponse{CaseId: req.CaseId, Status: "queued"}, nil
}
func (s *silentAgent) callsBy(caseID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.CaseId == caseID {
			n++
		}
	}
	return n
}

// flakyAgent returns ResultTimeout the first N calls, then 202. Used
// to drive the AgentUnreachable retry-once arm.
type flakyAgent struct {
	mu          sync.Mutex
	calls       atomic.Int32
	failFirst   int32
	calledWith  []*agentclient.DiagnoseRequest
}

func (f *flakyAgent) Diagnose(_ context.Context, req *agentclient.DiagnoseRequest) (*agentclient.DiagnoseResponse, *agentclient.AttemptError) {
	n := f.calls.Add(1)
	f.mu.Lock()
	f.calledWith = append(f.calledWith, req)
	f.mu.Unlock()
	if n <= f.failFirst {
		return nil, &agentclient.AttemptError{Class: agentclient.ResultTimeout}
	}
	return &agentclient.DiagnoseResponse{CaseId: req.CaseId, Status: "queued"}, nil
}

// startTestEnvUS3 is a copy of startTestEnv (US1's harness) that lets
// the caller inject custom AgentClient + reconciler tweaks. The US1
// fixture used global suite-scope vars; for US3 we want per-test
// agents so the tests can run in parallel and assert call counts.
func startTestEnvUS3(t *testing.T, agent controller.AgentClient, retryExtension time.Duration) (client.Client, *fakeSlack) {
	t.Helper()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir(t)},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := te.Start()
	if err != nil {
		t.Skipf("envtest unavailable: %v", err)
	}
	t.Cleanup(func() { _ = te.Stop() })

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodemedicv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	slack := &fakeSlack{}
	rec := &controller.NHDReconciler{
		Client:                 mgr.GetClient(),
		Recorder:               mgr.GetEventRecorderFor("nodemedic-controller-test"),
		Agent:                  agent,
		Slack:                  slack,
		MinConfidence:          0.7,
		MinEvidenceSources:     2,
		ClusterName:            "cf1z",
		Namespace:              "cf-monitoring",
		RetryDeadlineExtension: retryExtension,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}

	ctxMgr, cancel := context.WithCancel(context.Background())
	stopErr := make(chan error, 1)
	go func() { stopErr <- mgr.Start(ctxMgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctxMgr) {
		cancel()
		<-stopErr
		t.Fatal("cache sync failed")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopErr:
		case <-time.After(10 * time.Second):
		}
	})
	return mgr.GetClient(), slack
}

// crdDir returns the absolute path to config/nodemedic/crd from this
// test file's package dir. Mirrors the inline helper in
// us1_envtest_test.go.
func crdDir(t *testing.T) string {
	t.Helper()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(pwd, "..", "..", "..", "config", "nodemedic", "crd")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("crd dir %s: %v", p, err)
	}
	return p
}

// TestUS3_DeadlineExceeded_RetriesOnce_ThenCritical drives spec AC-5:
// agent stub stays silent (never writes status.phase=Diagnosed) →
// controller hits the deadline, bumps retry-count, extends the
// deadline + re-issues POST → second deadline hit goes terminal
// Failed + Critical Slack. Node MUST NOT be cordoned at any point.
func TestUS3_DeadlineExceeded_RetriesOnce_ThenCritical(t *testing.T) {
	agent := &silentAgent{}
	// 100ms retry extension keeps the test fast (~few seconds).
	cli, slack := startTestEnvUS3(t, agent, 100*time.Millisecond)
	ctx := context.Background()

	if err := cli.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-monitoring"},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ip-10-1-2-3.us-east-2.compute.internal",
			Labels: map[string]string{
				"cf.newrelic.com/cloud-provider": "aws",
				"topology.kubernetes.io/region":  "us-east-2",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-2a/i-0abc1234deadbeef"},
	}
	if err := cli.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// Deadline already in the past — first reconcile WILL hit the
	// retry path immediately on transition into Diagnosing.
	now := time.Now().UTC()
	pastDeadline := metav1.NewTime(now.Add(-1 * time.Second))
	caseID := "33333333-1234-4abc-89ef-1234567890ab"
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "us3-deadline-retry",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      caseID,
				NodeName:    node.Name,
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAWS,
				Region:      "us-east-2",
				InstanceId:  "i-0abc1234deadbeef",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "ConntrackSaturated",
					ObservedAt: metav1.NewTime(now),
				},
			},
			Budgets: nodemedicv1alpha1.BudgetsSpec{
				MaxTurns:     15,
				MaxBudgetUSD: "0.50",
				Deadline:     &pastDeadline,
			},
		},
	}
	if err := cli.Create(ctx, nhd); err != nil {
		t.Fatalf("create NHD: %v", err)
	}

	// Poll for terminal Failed (will go: empty → Diagnosing →
	// retry → Diagnosing → Failed).
	pollDeadline := time.Now().Add(20 * time.Second)
	var final nodemedicv1alpha1.NodeHealthDiagnosisAI
	for {
		if err := cli.Get(ctx, client.ObjectKeyFromObject(nhd), &final); err != nil {
			t.Fatalf("get nhd: %v", err)
		}
		if final.Status.Phase == nodemedicv1alpha1.PhaseFailed {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatalf("timed out: phase=%q action=%+v retry=%q",
				final.Status.Phase, final.Status.Action,
				final.Annotations[controller.RetryCountAnnotation])
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Node MUST NOT have been cordoned.
	var freshNode corev1.Node
	if err := cli.Get(ctx, client.ObjectKey{Name: node.Name}, &freshNode); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if freshNode.Spec.Unschedulable {
		t.Errorf("node was cordoned but should NOT have been on a Failed case")
	}

	// Retry annotation should record exactly 1 retry.
	rc := final.Annotations[controller.RetryCountAnnotation]
	if rc != "1" {
		t.Errorf("retry-count annotation = %q, want \"1\"", rc)
	}

	// AgentInvoked condition should be False with reason DeadlineExceeded.
	found := false
	for _, c := range final.Status.Conditions {
		if c.Type == "AgentInvoked" && c.Status == metav1.ConditionFalse {
			found = true
			if c.Reason != "DeadlineExceeded" {
				t.Errorf("AgentInvoked.reason = %q, want DeadlineExceeded", c.Reason)
			}
			if !strings.Contains(c.Message, "retried deadline") {
				t.Errorf("expected 'retried deadline' in message: %q", c.Message)
			}
		}
	}
	if !found {
		t.Errorf("AgentInvoked=False not found in %+v", final.Status.Conditions)
	}

	// Exactly one Critical Slack post.
	if posts := slack.posts.Load(); posts != 1 {
		t.Errorf("slack posts = %d, want 1 (Critical)", posts)
	}

	// Agent must have been called twice (initial + retry) — both with
	// the same caseId per FR-7.
	if got := agent.callsBy(caseID); got != 2 {
		t.Errorf("agent.Diagnose calls for caseId %s = %d, want 2", caseID, got)
	}
}

// TestUS3_AgentUnreachable_RetriesOnce_ThenCritical drives the
// transport-failure path: agent stub returns Timeout twice, controller
// retries once, second hit goes terminal Critical.
func TestUS3_AgentUnreachable_RetriesOnce_ThenCritical(t *testing.T) {
	agent := &flakyAgent{failFirst: 2} // both attempts fail
	cli, slack := startTestEnvUS3(t, agent, 100*time.Millisecond)
	ctx := context.Background()

	if err := cli.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-monitoring"},
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ip-10-1-2-4.us-east-2.compute.internal",
			Labels: map[string]string{
				"cf.newrelic.com/cloud-provider": "aws",
				"topology.kubernetes.io/region":  "us-east-2",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-2a/i-0def5678cafebabe"},
	}
	if err := cli.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	now := time.Now().UTC()
	mt := metav1.NewTime(now.Add(60 * time.Second))
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "us3-agent-unreachable",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "44444444-1234-4abc-89ef-1234567890ab",
				NodeName:    node.Name,
				ClusterName: "cf1z",
				Provider:    nodemedicv1alpha1.ProviderAWS,
				Region:      "us-east-2",
				InstanceId:  "i-0def5678cafebabe",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "ConntrackSaturated",
					ObservedAt: metav1.NewTime(now),
				},
			},
			Budgets: nodemedicv1alpha1.BudgetsSpec{
				MaxTurns:     15,
				MaxBudgetUSD: "0.50",
				Deadline:     &mt,
			},
		},
	}
	if err := cli.Create(ctx, nhd); err != nil {
		t.Fatalf("create NHD: %v", err)
	}

	pollDeadline := time.Now().Add(20 * time.Second)
	var final nodemedicv1alpha1.NodeHealthDiagnosisAI
	for {
		if err := cli.Get(ctx, client.ObjectKeyFromObject(nhd), &final); err != nil {
			t.Fatalf("get nhd: %v", err)
		}
		if final.Status.Phase == nodemedicv1alpha1.PhaseFailed {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatalf("timed out: phase=%q calls=%d", final.Status.Phase, agent.calls.Load())
		}
		time.Sleep(150 * time.Millisecond)
	}

	if rc, _ := strconv.Atoi(final.Annotations[controller.RetryCountAnnotation]); rc != 1 {
		t.Errorf("retry-count = %q, want 1", final.Annotations[controller.RetryCountAnnotation])
	}

	found := false
	for _, c := range final.Status.Conditions {
		if c.Type == "AgentInvoked" && c.Status == metav1.ConditionFalse && c.Reason == "AgentUnreachable" {
			found = true
		}
	}
	if !found {
		t.Errorf("AgentInvoked=False{AgentUnreachable} not found in %+v", final.Status.Conditions)
	}

	if posts := slack.posts.Load(); posts != 1 {
		t.Errorf("slack posts = %d, want 1 (Critical)", posts)
	}
	if agent.calls.Load() != 2 {
		t.Errorf("agent calls = %d, want 2 (initial + retry)", agent.calls.Load())
	}

	// Compile-time assertion that the notifier package is reachable —
	// otherwise an unused-import lint would surface; this also future-
	// proofs against accidental import drift.
	_ = notifier.PostResult{}
}
