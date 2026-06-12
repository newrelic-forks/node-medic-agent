//go:build integration

/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Integration tests for the NodeMedic controller running against a
// real apiserver+etcd via sigs.k8s.io/controller-runtime/pkg/envtest.
// Gated behind `//go:build integration` so `go test ./...` stays fast;
// run via `make nodemedic-envtest` which sets KUBEBUILDER_ASSETS
// before invoking `go test -tags=integration`.
package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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

// fakeAgent records calls and always returns 202.
type fakeAgent struct {
	mu    sync.Mutex
	calls []*agentclient.DiagnoseRequest
}

func (f *fakeAgent) Diagnose(_ context.Context, req *agentclient.DiagnoseRequest) (*agentclient.DiagnoseResponse, *agentclient.AttemptError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return &agentclient.DiagnoseResponse{CaseId: req.CaseId, Status: "queued"}, nil
}
func (f *fakeAgent) Calls() []*agentclient.DiagnoseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*agentclient.DiagnoseRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeSlack counts Post calls; always succeeds.
type fakeSlack struct{ posts atomic.Int32 }

func (f *fakeSlack) Post(_ context.Context, _ []byte) notifier.PostResult {
	f.posts.Add(1)
	return notifier.PostResult{Posted: true, Attempts: 1}
}

type envSuite struct {
	cfg    *rest.Config
	cli    client.Client
	agent  *fakeAgent
	slack  *fakeSlack
}

func startTestEnv(t *testing.T) *envSuite {
	t.Helper()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// pwd is the package dir (internal/nodemedic/controller); CRD
	// directory is three levels up at config/nodemedic/crd.
	crdPath := filepath.Join(pwd, "..", "..", "..", "config", "nodemedic", "crd")
	if _, err := os.Stat(crdPath); err != nil {
		t.Fatalf("crd dir %s: %v", crdPath, err)
	}

	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := te.Start()
	if err != nil {
		t.Skipf("envtest unavailable (set KUBEBUILDER_ASSETS via `make nodemedic-envtest`): %v", err)
	}
	t.Cleanup(func() {
		if err := te.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodemedicv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	agent := &fakeAgent{}
	slack := &fakeSlack{}

	rec := &controller.NHDReconciler{
		Client:             mgr.GetClient(),
		Recorder:           mgr.GetEventRecorderFor("nodemedic-controller-test"),
		Agent:              agent,
		Slack:              slack,
		MinConfidence:      0.7,
		MinEvidenceSources: 2,
		ClusterName:        "test-odd-wire",
		Namespace:          "cf-monitoring",
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopErr := make(chan error, 1)
	go func() { stopErr <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		<-stopErr
		t.Fatal("cache sync failed")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopErr:
		case <-time.After(10 * time.Second):
			t.Logf("manager did not stop within 10s")
		}
	})

	return &envSuite{cfg: cfg, cli: mgr.GetClient(), agent: agent, slack: slack}
}

// TestUS1_HappyPath_GatePassCordons drives spec AC-3: phase=Diagnosed
// with a passing diagnosis → controller cordons + posts Slack
// (Applied) → terminal phase=Acted.
func TestUS1_HappyPath_GatePassCordons(t *testing.T) {
	suite := startTestEnv(t)
	ctx := context.Background()

	if err := suite.cli.Create(ctx, &corev1.Namespace{
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
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-east-2a/i-0abc1234deadbeef",
		},
	}
	if err := suite.cli.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	now := time.Now().UTC()
	deadline := now.Add(60 * time.Second)
	mt := metav1.NewTime(deadline)
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "us1-happy-path",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c",
				NodeName:    node.Name,
				ClusterName: "test-odd-wire",
				Provider:    nodemedicv1alpha1.ProviderAWS,
				Region:      "us-east-2",
				InstanceId:  "i-0abc1234deadbeef",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "ConntrackSaturated",
					Reason:     "ConntrackHigh",
					Message:    "nf_conntrack_count=262100 max=262144",
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
	if err := suite.cli.Create(ctx, nhd); err != nil {
		t.Fatalf("create NHD: %v", err)
	}

	patchBase := nhd.DeepCopy()
	nhd.Status = nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
		Phase: nodemedicv1alpha1.PhaseDiagnosed,
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RootCause:   "conntrack table exhausted",
			RCACategory: nodemedicv1alpha1.RCACategory("Conntrack"),
			Confidence:  0.85,
			Evidence: []nodemedicv1alpha1.EvidenceItem{
				{Source: "nrql"}, {Source: "ssh"}, {Source: "kubectl"},
			},
			Recommendation: &nodemedicv1alpha1.Recommendation{
				Action: nodemedicv1alpha1.RecommendationActionCordon,
			},
		},
	}
	if err := suite.cli.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		t.Fatalf("status patch: %v", err)
	}

	// Poll for phase=Acted + cordon (15s budget — controller's
	// reconcile loop reacts in <100ms, but the workqueue + cache
	// settle pads that).
	pollDeadline := time.Now().Add(15 * time.Second)
	var final nodemedicv1alpha1.NodeHealthDiagnosisAI
	var finalNode corev1.Node
	for {
		if err := suite.cli.Get(ctx, client.ObjectKeyFromObject(nhd), &final); err != nil {
			t.Fatalf("get nhd: %v", err)
		}
		if err := suite.cli.Get(ctx, client.ObjectKey{Name: node.Name}, &finalNode); err != nil {
			t.Fatalf("get node: %v", err)
		}
		if final.Status.Phase == nodemedicv1alpha1.PhaseActed && finalNode.Spec.Unschedulable {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatalf("timed out: phase=%q unschedulable=%v action=%+v",
				final.Status.Phase, finalNode.Spec.Unschedulable, final.Status.Action)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if final.Status.Action == nil ||
		final.Status.Action.Decision != nodemedicv1alpha1.ActionDecisionApplied {
		t.Errorf("action.decision = %+v, want Applied", final.Status.Action)
	}
	if final.Status.Action.Operation != nodemedicv1alpha1.ActionOperationCordon {
		t.Errorf("action.operation = %q, want cordon", final.Status.Action.Operation)
	}
	if !finalNode.Spec.Unschedulable {
		t.Errorf("node should be cordoned")
	}
	if posts := suite.slack.posts.Load(); posts != 1 {
		t.Errorf("slack posts = %d, want 1", posts)
	}
	// Agent should NOT have been called — we hand-injected Diagnosed.
	if calls := len(suite.agent.Calls()); calls != 0 {
		t.Errorf("agent.Diagnose calls = %d, want 0", calls)
	}
}

// TestUS1_PendingPhasePostsToAgent covers ""→Diagnosing: a fresh NHD
// with empty status should drive the controller to call the agent.
func TestUS1_PendingPhasePostsToAgent(t *testing.T) {
	suite := startTestEnv(t)
	ctx := context.Background()

	if err := suite.cli.Create(ctx, &corev1.Namespace{
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
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-east-2a/i-0def5678cafebabe",
		},
	}
	if err := suite.cli.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	now := time.Now().UTC()
	mt := metav1.NewTime(now.Add(60 * time.Second))
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "us1-pending-arm",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "12345678-1234-4abc-89ef-1234567890ab",
				NodeName:    node.Name,
				ClusterName: "test-odd-wire",
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
	if err := suite.cli.Create(ctx, nhd); err != nil {
		t.Fatalf("create NHD: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if calls := len(suite.agent.Calls()); calls >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent.Diagnose was not called within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var final nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := suite.cli.Get(ctx, client.ObjectKeyFromObject(nhd), &final); err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status.Phase != nodemedicv1alpha1.PhaseDiagnosing {
		t.Errorf("phase = %q, want Diagnosing", final.Status.Phase)
	}
	found := false
	for _, c := range final.Status.Conditions {
		if c.Type == "AgentInvoked" && c.Status == metav1.ConditionTrue && c.Reason == "Posted" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AgentInvoked=True{Posted} not found in %+v", final.Status.Conditions)
	}
	calls := suite.agent.Calls()
	if calls[0].CaseId != nhd.Spec.Case.CaseId {
		t.Errorf("agent saw caseId=%q, want %q", calls[0].CaseId, nhd.Spec.Case.CaseId)
	}
}
