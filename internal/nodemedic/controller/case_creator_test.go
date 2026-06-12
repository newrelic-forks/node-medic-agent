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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

func nhdScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := nodemedicv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func eksNode() *corev1.Node {
	return &corev1.Node{
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
}

func TestCreateCase_HappyPath(t *testing.T) {
	t.Parallel()
	node := eksNode()
	scheme := nhdScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

	ts := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	deadline := ts.Add(60 * time.Second)
	trigger := Trigger{
		Node:          node,
		ConditionType: "ConntrackSaturated",
		Reason:        "ConntrackHigh",
		Message:       "nf_conntrack_count=262100",
		ObservedAt:    ts,
	}

	nhd, err := CreateCase(context.Background(), cli, trigger,
		"test-odd-wire", "cf-monitoring",
		15, "0.50", deadline,
	)
	if err != nil {
		t.Fatalf("CreateCase: %v", err)
	}

	if nhd.Spec.Case.NodeName != node.Name {
		t.Errorf("NodeName = %q, want %q", nhd.Spec.Case.NodeName, node.Name)
	}
	if nhd.Spec.Case.Provider != nodemedicv1alpha1.ProviderAWS {
		t.Errorf("Provider = %q, want aws", nhd.Spec.Case.Provider)
	}
	if nhd.Spec.Case.Region != "us-east-2" {
		t.Errorf("Region = %q", nhd.Spec.Case.Region)
	}
	if nhd.Spec.Case.InstanceId != "i-0abc1234deadbeef" {
		t.Errorf("InstanceId = %q", nhd.Spec.Case.InstanceId)
	}
	if nhd.Spec.Case.CaseId == "" {
		t.Error("CaseId not generated")
	}
	wantName := NameForCase(node.Name, ts)
	if nhd.Name != wantName {
		t.Errorf("nhd.Name = %q, want %q", nhd.Name, wantName)
	}
	if nhd.Namespace != "cf-monitoring" {
		t.Errorf("Namespace = %q", nhd.Namespace)
	}

	// Verify it actually persisted.
	var fresh nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := cli.Get(context.Background(),
		client.ObjectKey{Name: nhd.Name, Namespace: nhd.Namespace},
		&fresh); err != nil {
		t.Fatalf("get after create: %v", err)
	}
}

func TestCreateCase_IdempotentOnAlreadyExists(t *testing.T) {
	t.Parallel()
	node := eksNode()
	scheme := nhdScheme(t)

	// Pre-create an NHD with the deterministic name a future case
	// would compute.
	ts := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	wantName := NameForCase(node.Name, ts)
	preexisting := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wantName,
			Namespace: "cf-monitoring",
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      "11111111-1111-4111-9111-111111111111",
				NodeName:    node.Name,
				ClusterName: "test-odd-wire",
				Provider:    nodemedicv1alpha1.ProviderAWS,
				Region:      "us-east-2",
				InstanceId:  "i-0abc1234deadbeef",
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       "ConntrackSaturated",
					ObservedAt: metav1.NewTime(ts),
				},
			},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, preexisting).
		Build()

	trigger := Trigger{
		Node:          node,
		ConditionType: "ConntrackSaturated",
		ObservedAt:    ts,
	}
	got, err := CreateCase(context.Background(), cli, trigger,
		"test-odd-wire", "cf-monitoring",
		15, "0.50", ts.Add(60*time.Second),
	)
	if err != nil {
		t.Fatalf("CreateCase: %v", err)
	}
	if got.Spec.Case.CaseId != "11111111-1111-4111-9111-111111111111" {
		t.Errorf("expected to return preexisting NHD's caseId, got %q",
			got.Spec.Case.CaseId)
	}
}

func TestCreateCase_MetadataResolutionFailures(t *testing.T) {
	t.Parallel()
	scheme := nhdScheme(t)

	cases := []struct {
		name      string
		mutate    func(n *corev1.Node)
		wantField string
	}{
		{
			name: "missing region label",
			mutate: func(n *corev1.Node) {
				delete(n.Labels, "topology.kubernetes.io/region")
				delete(n.Labels, "failure-domain.beta.kubernetes.io/region")
			},
			wantField: "region",
		},
		{
			name: "empty providerID with no provider label",
			mutate: func(n *corev1.Node) {
				delete(n.Labels, "cf.newrelic.com/cloud-provider")
				n.Spec.ProviderID = ""
			},
			wantField: "provider",
		},
		{
			name: "malformed AWS providerID",
			mutate: func(n *corev1.Node) {
				n.Spec.ProviderID = "aws:///us-east-2a/notaninstance"
			},
			wantField: "instanceId",
		},
		{
			name: "azure provider not yet supported in US1",
			mutate: func(n *corev1.Node) {
				n.Labels["cf.newrelic.com/cloud-provider"] = "azure"
				n.Spec.ProviderID = "azure:///subscriptions/x/resourceGroups/y/providers/Microsoft.Compute/virtualMachines/vm"
			},
			wantField: "instanceId",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := eksNode()
			tc.mutate(node)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

			trigger := Trigger{
				Node:          node,
				ConditionType: "ConntrackSaturated",
				ObservedAt:    time.Now(),
			}
			_, err := CreateCase(context.Background(), cli, trigger,
				"test-odd-wire", "cf-monitoring",
				15, "0.50", time.Now().Add(60*time.Second),
			)
			if err == nil {
				t.Fatal("expected error")
			}
			var mre *MetadataResolutionError
			if !errors.As(err, &mre) {
				t.Fatalf("err = %v, want MetadataResolutionError", err)
			}
			if mre.Field != tc.wantField {
				t.Errorf("Field = %q, want %q (detail=%q)", mre.Field, tc.wantField, mre.Detail)
			}
		})
	}
}
