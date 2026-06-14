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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestCordon_HappyPath(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       corev1.NodeSpec{Unschedulable: false},
	}
	cli := newFakeClient(t, node)

	res := Cordon(context.Background(), cli, node.DeepCopy(), "node-a")
	if res.Err != nil {
		t.Fatalf("err = %v", res.Err)
	}
	if !res.Patched {
		t.Errorf("want Patched=true, got %+v", res)
	}

	var fresh corev1.Node
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "node-a"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !fresh.Spec.Unschedulable {
		t.Errorf("expected node Unschedulable=true after cordon")
	}
}

func TestCordon_AlreadyCordoned(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	cli := newFakeClient(t, node)

	res := Cordon(context.Background(), cli, node.DeepCopy(), "node-a")
	if res.Err != nil {
		t.Fatalf("err = %v", res.Err)
	}
	if res.Patched {
		t.Errorf("want Patched=false (idempotent), got %+v", res)
	}
}

func TestCordon_RefusesWrongNode(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	}
	cli := newFakeClient(t, node)

	res := Cordon(context.Background(), cli, node.DeepCopy(), "node-b")
	if !res.Refused {
		t.Errorf("expected Refused=true, got %+v", res)
	}
	if !errors.Is(res.Err, ErrWrongNode) {
		t.Errorf("err = %v, want ErrWrongNode", res.Err)
	}
	if res.Patched {
		t.Errorf("must not patch on refusal")
	}
}

func TestCordon_NilNode(t *testing.T) {
	t.Parallel()
	cli := newFakeClient(t)

	res := Cordon(context.Background(), cli, nil, "node-a")
	if res.Err == nil {
		t.Error("expected error on nil node")
	}
}
