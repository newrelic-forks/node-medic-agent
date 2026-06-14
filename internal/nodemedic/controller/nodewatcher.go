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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// NodeWatcher detects watched-condition transitions to True on Nodes
// and creates one NHD per (node, condition) — FR-1 + FR-2 + FR-3.
//
// A controller-runtime Reconciler keyed on Node objects, gated by an
// Update predicate that only enqueues when a watched condition's
// status transitions to True. Reconcile then checks each watched type
// in order, runs the in-memory debounce (FR-1 30 s window), and
// dispatches to CreateCase per allowed match.
type NodeWatcher struct {
	client.Client
	Recorder record.EventRecorder

	WatchedConditions []string
	Debounce          *DebounceMap
	ClusterName       string
	Namespace         string
	MaxTurns          int32
	MaxBudgetUSD      string
	DeadlineWindow    time.Duration

	// Now is injected for tests; defaults to time.Now in main.go.
	Now func() time.Time
}

// SetupWithManager registers the watcher with the manager. The
// predicate filters Update events down to "transitioned-to-True for a
// watched condition" so we don't reconcile on every Node heartbeat
// (which would be O(nodes-per-cluster) churn per minute).
func (nw *NodeWatcher) SetupWithManager(mgr ctrl.Manager) error {
	if nw.Debounce == nil {
		return errors.New("NodeWatcher.Debounce is required")
	}
	if len(nw.WatchedConditions) == 0 {
		return errors.New("NodeWatcher.WatchedConditions is required")
	}
	watched := nw.watchedSet()
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodemedic-nodewatcher").
		For(&corev1.Node{}, builder.WithPredicates(transitionPredicate(watched))).
		Complete(nw)
}

// Reconcile is invoked when the predicate matches. We iterate every
// watched type rather than tracking the specific condition that
// changed — the predicate guarantees AT LEAST one matched, the
// debounce filters duplicates, and creating the NHD is idempotent on
// AlreadyExists (FR-3).
func (nw *NodeWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("nodewatcher").WithValues("node", req.Name)

	var node corev1.Node
	if err := nw.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := nw.now()
	for _, condType := range nw.WatchedConditions {
		cond := findCondition(node.Status.Conditions, condType)
		if cond == nil || cond.Status != corev1.ConditionTrue {
			continue
		}
		// Safety net against picking up stale True conditions on
		// controller restart: only act if the transition is recent
		// (within 2x the debounce window — generous enough that we
		// don't miss a slow first reconcile, tight enough that we
		// don't replay year-old conditions).
		if !cond.LastTransitionTime.IsZero() &&
			now.Sub(cond.LastTransitionTime.Time) > 2*nw.Debounce.Window() {
			continue
		}

		key := DebounceKey{Node: node.Name, Type: condType}
		if !nw.Debounce.Allow(key, now) {
			logger.V(1).Info("debounced", "type", condType)
			continue
		}

		observed := cond.LastTransitionTime.Time
		if observed.IsZero() {
			observed = now
		}
		deadline := observed.Add(nw.DeadlineWindow)

		trigger := Trigger{
			Node:          &node,
			ConditionType: condType,
			Reason:        cond.Reason,
			Message:       cond.Message,
			ObservedAt:    observed,
		}
		nhd, err := CreateCase(ctx, nw.Client, trigger,
			nw.ClusterName, nw.Namespace,
			nw.MaxTurns, nw.MaxBudgetUSD, deadline,
		)
		if err != nil {
			var mre *MetadataResolutionError
			if errors.As(err, &mre) {
				nw.Recorder.Eventf(&node, corev1.EventTypeWarning, "MetadataResolutionFailed",
					"%s: %s", mre.Field, mre.Detail)
				continue // try next watched type; don't requeue the Node
			}
			return ctrl.Result{}, fmt.Errorf("create case (%s): %w", condType, err)
		}
		nw.Recorder.Eventf(&node, corev1.EventTypeNormal, "CaseCreated",
			"NodeHealthDiagnosisAI/%s/%s for %s", nhd.Namespace, nhd.Name, condType)
	}
	return ctrl.Result{}, nil
}

func (nw *NodeWatcher) now() time.Time {
	if nw.Now != nil {
		return nw.Now()
	}
	return time.Now()
}

func (nw *NodeWatcher) watchedSet() map[string]struct{} {
	out := make(map[string]struct{}, len(nw.WatchedConditions))
	for _, t := range nw.WatchedConditions {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// transitionPredicate returns a predicate that lets Update events
// through only when at least one watched condition transitioned from
// {False, Unknown, missing} to True between old and new. Create events
// pass if the new Node already has a True watched condition (covers
// controller startup against pre-existing fault state).
func transitionPredicate(watched map[string]struct{}) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			node, ok := e.Object.(*corev1.Node)
			if !ok {
				return false
			}
			return hasTrueWatchedCondition(node, watched)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}
			return hasTransitionToTrue(oldNode, newNode, watched)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func hasTrueWatchedCondition(node *corev1.Node, watched map[string]struct{}) bool {
	for _, c := range node.Status.Conditions {
		if _, ok := watched[string(c.Type)]; !ok {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func hasTransitionToTrue(oldNode, newNode *corev1.Node, watched map[string]struct{}) bool {
	for _, n := range newNode.Status.Conditions {
		if _, ok := watched[string(n.Type)]; !ok {
			continue
		}
		if n.Status != corev1.ConditionTrue {
			continue
		}
		// New is True — was the old one not True?
		o := findCondition(oldNode.Status.Conditions, string(n.Type))
		if o == nil || o.Status != corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func findCondition(conds []corev1.NodeCondition, condType string) *corev1.NodeCondition {
	for i := range conds {
		if string(conds[i].Type) == condType {
			return &conds[i]
		}
	}
	return nil
}
