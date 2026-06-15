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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrWrongNode is returned by Cordon when the Node's name does not
// match the NHD's spec.case.nodeName. FR-8 belt-and-suspenders.
var ErrWrongNode = errors.New("node name does not match NHD spec.case.nodeName")

// MLCSkipDeletionAnnotation pauses MLC's NodeReconciler from rotating a
// cordoned worker node (see machine-lifecycle-controller#466). NodeMedic
// stamps this alongside `.spec.unschedulable=true` so an on-call engineer
// can SSH in and inspect the cordoned node before MLC reclaims the VM.
//
// The setter (NodeMedic) does NOT auto-clear the annotation — the
// engineer is expected to remove it once investigation is done, at which
// point MLC's next reconcile proceeds with normal deletion. That puts a
// human in the loop on the destruction side, which is the whole point.
const MLCSkipDeletionAnnotation = "machine-lifecycle.newrelic.com/skipDeletion"

// Cordon patches Node.spec.unschedulable=true and stamps the
// MLC skipDeletion annotation in the same write. Idempotent — if both
// are already in place, returns Patched=false with no error. Refuses if
// expectedNodeName doesn't match node.Name (FR-8 safety belt). The
// function ONLY mutates spec.unschedulable + the skipDeletion
// annotation; nothing else.
//
// Constitution Article I.2: this is the single mutation surface. No
// drain, no eviction, no pod deletion.
func Cordon(ctx context.Context, c client.Client, node *corev1.Node, expectedNodeName string) CordonResult {
	if node == nil {
		return CordonResult{Err: errors.New("nil node")}
	}
	if node.Name != expectedNodeName {
		return CordonResult{
			Refused: true,
			Err: fmt.Errorf("%w: node=%q expected=%q",
				ErrWrongNode, node.Name, expectedNodeName),
		}
	}
	annotationAlreadySet := node.Annotations[MLCSkipDeletionAnnotation] == "true"
	if node.Spec.Unschedulable && annotationAlreadySet {
		// Already cordoned and pinned by something else — or by us on a
		// previous reconcile that crashed before status update (FR-11).
		return CordonResult{Patched: false}
	}
	orig := node.DeepCopy()
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[MLCSkipDeletionAnnotation] = "true"
	if err := c.Patch(ctx, node, client.MergeFrom(orig)); err != nil {
		return CordonResult{Err: fmt.Errorf("patch node.spec.unschedulable: %w", err)}
	}
	return CordonResult{Patched: true}
}
