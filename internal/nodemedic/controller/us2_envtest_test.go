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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// TestUS2_GateFail_NoCordon_HumanInLoop verifies spec AC-4: an NHD
// reaches phase=Diagnosed with a diagnosis that fails the confidence
// gate → controller routes to HumanInLoop, posts the gate-fail Slack
// message, and does NOT touch Node.spec.unschedulable.
func TestUS2_GateFail_NoCordon_HumanInLoop(t *testing.T) {
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
	mt := metav1.NewTime(now.Add(60 * time.Second))
	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "us2-gate-fail",
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "1c2d3e4f-5678-4901-9234-5e6f7a8b9c0d",
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
				Deadline:     &mt,
			},
		},
	}
	if err := suite.cli.Create(ctx, nhd); err != nil {
		t.Fatalf("create NHD: %v", err)
	}

	// Diagnosis fails BOTH non-action clauses: confidence 0.5 < 0.7
	// AND only 1 distinct source < 2.
	patchBase := nhd.DeepCopy()
	nhd.Status = nodemedicv1alpha1.NodeHealthDiagnosisAIStatus{
		Phase: nodemedicv1alpha1.PhaseDiagnosed,
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RootCause:   "conntrack climbing but uncertain",
			RCACategory: nodemedicv1alpha1.RCACategory("Conntrack"),
			Confidence:  0.5,
			Evidence: []nodemedicv1alpha1.EvidenceItem{
				{Source: "nrql"},
			},
			Recommendation: &nodemedicv1alpha1.Recommendation{
				Action: nodemedicv1alpha1.RecommendationActionCordon,
			},
		},
	}
	if err := suite.cli.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		t.Fatalf("status patch: %v", err)
	}

	pollDeadline := time.Now().Add(15 * time.Second)
	var final nodemedicv1alpha1.NodeHealthDiagnosisAI
	for {
		if err := suite.cli.Get(ctx, client.ObjectKeyFromObject(nhd), &final); err != nil {
			t.Fatalf("get nhd: %v", err)
		}
		if final.Status.Phase == nodemedicv1alpha1.PhaseActed {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatalf("timed out: phase=%q action=%+v",
				final.Status.Phase, final.Status.Action)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if final.Status.Action == nil ||
		final.Status.Action.Decision != nodemedicv1alpha1.ActionDecisionHumanInLoop {
		t.Errorf("action.decision = %+v, want HumanInLoop", final.Status.Action)
	}
	if final.Status.Action.Operation != nodemedicv1alpha1.ActionOperationNoop {
		t.Errorf("action.operation = %q, want noop", final.Status.Action.Operation)
	}

	// Cordon MUST NOT have happened.
	var freshNode corev1.Node
	if err := suite.cli.Get(ctx, client.ObjectKey{Name: node.Name}, &freshNode); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if freshNode.Spec.Unschedulable {
		t.Errorf("node was cordoned but should NOT have been (gate failed)")
	}

	// Slack should have received exactly one POST.
	if posts := suite.slack.posts.Load(); posts != 1 {
		t.Errorf("slack posts = %d, want 1 (HumanInLoop)", posts)
	}

	// ActionApplied condition with reason=HumanInLoop carries the
	// gate's reason in its message — operator's audit trail.
	found := false
	for _, c := range final.Status.Conditions {
		if c.Type == "ActionApplied" && c.Reason == "HumanInLoop" {
			found = true
			if !strings.Contains(c.Message, "confidence 0.50 < min 0.70") {
				t.Errorf("ActionApplied.message missing confidence-clause reason: %q", c.Message)
			}
			if !strings.Contains(c.Message, "distinct sources 1 < min 2") {
				t.Errorf("ActionApplied.message missing sources-clause reason: %q", c.Message)
			}
		}
	}
	if !found {
		t.Errorf("ActionApplied=%v condition not found in %+v", "HumanInLoop", final.Status.Conditions)
	}
}
