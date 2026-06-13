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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/nodemedic/agentclient"
	"k8s.io/node-problem-detector/internal/nodemedic/metrics"
	"k8s.io/node-problem-detector/internal/nodemedic/notifier"
)

// AgentClient is the subset of agentclient.Client the reconciler
// depends on. Defined as an interface so envtests can inject a fake.
type AgentClient interface {
	Diagnose(ctx context.Context, req *agentclient.DiagnoseRequest) (*agentclient.DiagnoseResponse, *agentclient.AttemptError)
}

// SlackNotifier is the subset of notifier.Slack the reconciler
// depends on. Same interface-for-testability rationale.
type SlackNotifier interface {
	Post(ctx context.Context, payload []byte) notifier.PostResult
}

// NHDReconciler drives the NHD phase machine (FR-5). One instance per
// manager; safe under controller-runtime's concurrent dispatch.
type NHDReconciler struct {
	client.Client
	Scheme   any                   // kept for completeness; unused in v1
	Recorder record.EventRecorder

	Agent       AgentClient
	Slack       SlackNotifier
	BearerToken string // for the agent client, kept here so we don't restamp it per request

	MinConfidence      float64
	MinEvidenceSources int
	ClusterName        string
	Namespace          string

	// RetryDeadlineExtension is how much we push spec.budgets.deadline
	// forward when retrying a DeadlineExceeded case. Default 60s in
	// main.go; envtests override to a small value so the retry-then-
	// terminal flow finishes quickly.
	RetryDeadlineExtension time.Duration

	// Now is injected for tests; defaults to time.Now in main.go.
	Now func() time.Time
}

// RetryCountAnnotation tracks how many controller-level retries this
// case has burned. FR-7 caps it at 1 — second hit goes terminal
// `Failed` + Critical Slack. Storing on metadata.annotations makes
// the count survive controller restarts (FR-11 idempotency edge).
const RetryCountAnnotation = "nodemedic.cf.newrelic.com/retry-count"

// retryDeadlineExtensionDefault is what main.go uses if Reconciler is
// constructed without an explicit RetryDeadlineExtension. Spec FR-3
// sets the original deadline at observedAt+60s; we mirror that on
// retry.
const retryDeadlineExtensionDefault = 60 * time.Second

// getRetryCount reads the annotation; missing or malformed → 0.
func getRetryCount(nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI) int {
	if nhd.Annotations == nil {
		return 0
	}
	v := nhd.Annotations[RetryCountAnnotation]
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// SetupWithManager registers this reconciler against NHD CRs.
func (r *NHDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodemedic-nhd").
		For(&nodemedicv1alpha1.NodeHealthDiagnosisAI{}).
		Complete(r)
}

// requeueDuringDiagnosis is the polling cadence while we wait for the
// agent to write status.diagnosis. Capped by deadline in the phase
// machine itself.
const requeueDuringDiagnosis = 10 * time.Second

// Reconcile is the FR-5 phase machine.
//
// Phase transitions implemented in US1:
//
//	""/Pending  -> POST /diagnose -> Diagnosing
//	Diagnosed  -> gate(pass)      -> cordon + Slack(Applied) -> Acted
//	Diagnosed  -> gate(fail/skip) -> noop (no cordon)        -> Acted
//	Diagnosing -> deadline        -> Failed                  (Phase 5 adds retry + Critical Slack)
//	Acted/Failed -> terminal
//
// Phase 4 (US2) extends the gate-fail arm with BuildHumanInLoop Slack.
// Phase 5 (US3) extends the deadline arm with retry + BuildCritical.
func (r *NHDReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("nhd").WithValues("nhd", req.NamespacedName)

	var nhd nodemedicv1alpha1.NodeHealthDiagnosisAI
	if err := r.Get(ctx, req.NamespacedName, &nhd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := r.now()

	switch nhd.Status.Phase {
	case "", nodemedicv1alpha1.PhasePending:
		return r.invokeAgent(ctx, logger, &nhd)

	case nodemedicv1alpha1.PhaseDiagnosing:
		if d := nhd.Spec.Budgets.Deadline; d != nil && now.After(d.Time) {
			logger.Info("deadline exceeded",
				"deadline", d.Time, "now", now,
				"retryCount", getRetryCount(&nhd))
			return r.handleDeadlineExceeded(ctx, &nhd)
		}
		return ctrl.Result{RequeueAfter: requeueDuringDiagnosis}, nil

	case nodemedicv1alpha1.PhaseDiagnosed:
		return r.handleDiagnosed(ctx, logger, &nhd)

	case nodemedicv1alpha1.PhaseActed, nodemedicv1alpha1.PhaseFailed:
		return ctrl.Result{}, nil // terminal

	default:
		// Evaluating / Evaluated — reserved for the post-hackathon
		// eval agent. The controller does nothing with them in v1.
		logger.Info("unknown or eval-reserved phase; not requeuing", "phase", nhd.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *NHDReconciler) invokeAgent(
	ctx context.Context,
	logger interface{ Info(string, ...any); Error(error, string, ...any) },
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
) (ctrl.Result, error) {
	deadline := 60
	if d := nhd.Spec.Budgets.Deadline; d != nil {
		deadline = int(d.Time.Sub(nhd.Spec.Case.Trigger.ObservedAt.Time).Seconds())
		if deadline <= 0 {
			deadline = 60
		}
	}

	req := &agentclient.DiagnoseRequest{
		CaseId:      nhd.Spec.Case.CaseId,
		NodeName:    nhd.Spec.Case.NodeName,
		ClusterName: nhd.Spec.Case.ClusterName,
		Provider:    string(nhd.Spec.Case.Provider),
		Region:      nhd.Spec.Case.Region,
		InstanceId:  nhd.Spec.Case.InstanceId,
		Trigger: agentclient.DiagnoseTrigger{
			Type:       nhd.Spec.Case.Trigger.Type,
			Reason:     nhd.Spec.Case.Trigger.Reason,
			Message:    nhd.Spec.Case.Trigger.Message,
			ObservedAt: nhd.Spec.Case.Trigger.ObservedAt.UTC().Format(time.RFC3339),
		},
		Budgets: agentclient.DiagnoseBudgets{
			MaxTurns:     int(nhd.Spec.Budgets.MaxTurns),
			MaxBudgetUSD: nhd.Spec.Budgets.MaxBudgetUSD,
			DeadlineSec:  deadline,
		},
	}

	resp, attErr := r.Agent.Diagnose(ctx, req)
	if attErr != nil {
		metrics.AgentPostTotal.WithLabelValues(attErr.Class.String()).Inc()
		switch attErr.Class {
		case agentclient.ResultBadRequest:
			// Malformed request — retrying won't help. Terminal Critical.
			return r.criticalFailure(ctx, nhd, "BadRequest", attErr.Error())
		case agentclient.ResultUnauthorized:
			// Bad token — same; rotating mid-reconcile is out of scope.
			return r.criticalFailure(ctx, nhd, "Unauthorized", attErr.Error())
		case agentclient.ResultRetry, agentclient.ResultServer, agentclient.ResultTimeout:
			// Retry-eligible (FR-7). Burn the controller-level retry
			// once; on a second hit go terminal Critical.
			return r.handleAgentUnreachable(ctx, nhd, attErr)
		default:
			return r.criticalFailure(ctx, nhd, "AgentError", attErr.Error())
		}
	}
	logger.Info("agent accepted POST /diagnose", "status", resp.Status, "caseId", resp.CaseId)
	metrics.AgentPostTotal.WithLabelValues(agentclient.ResultOK.String()).Inc()

	if err := r.transitionPhase(ctx, nhd, nodemedicv1alpha1.PhaseDiagnosing,
		metav1.Condition{
			Type:    "AgentInvoked",
			Status:  metav1.ConditionTrue,
			Reason:  "Posted",
			Message: fmt.Sprintf("agent returned status=%q", resp.Status),
		}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueDuringDiagnosis}, nil
}

func (r *NHDReconciler) handleDiagnosed(
	ctx context.Context,
	logger interface{ Info(string, ...any) },
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
) (ctrl.Result, error) {
	gate := ConfidenceGate(nhd.Status.Diagnosis, r.MinConfidence, r.MinEvidenceSources)
	logger.Info("gate evaluated", "outcome", gate.Outcome.String(), "reason", gate.Reason,
		"confidence", gate.Confidence, "distinctSources", gate.DistinctSources)

	switch gate.Outcome {
	case GatePass:
		return r.applyPath(ctx, nhd, gate)
	case GateFail, GateSkip:
		// US2 (Phase 4) adds the BuildHumanInLoop Slack post here.
		// For US1 we still record the decision so the demo path
		// reaches Acted.
		return r.humanInLoopPath(ctx, nhd, gate)
	}
	return ctrl.Result{}, fmt.Errorf("unknown gate outcome %v", gate.Outcome)
}

func (r *NHDReconciler) applyPath(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	gate GateResult,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Cordon.
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nhd.Spec.Case.NodeName}, &node); err != nil {
		metrics.CordonTotal.WithLabelValues(metrics.CordonErr).Inc()
		return r.markFailed(ctx, nhd, "NodeNotFound",
			fmt.Sprintf("could not get Node %q: %v", nhd.Spec.Case.NodeName, err))
	}
	cr := Cordon(ctx, r.Client, &node, nhd.Spec.Case.NodeName)
	if cr.Err != nil {
		metrics.CordonTotal.WithLabelValues(metrics.CordonErr).Inc()
		if errors.Is(cr.Err, ErrWrongNode) {
			r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "WrongNodeRefused",
				"refused to cordon %q (NHD references %q)", node.Name, nhd.Spec.Case.NodeName)
		}
		return r.markFailed(ctx, nhd, "CordonFailed", cr.Err.Error())
	}
	metrics.CordonTotal.WithLabelValues(metrics.CordonOK).Inc()
	logger.Info("cordon ok", "node", node.Name, "patched", cr.Patched)

	// 2. Slack.
	now := r.now()
	payload, err := notifier.BuildApplied(notifier.AppliedInput{
		NodeName:    nhd.Spec.Case.NodeName,
		ClusterName: nhd.Spec.Case.ClusterName,
		Namespace:   nhd.Namespace,
		NHDName:     nhd.Name,
		Diagnosis:   nhd.Status.Diagnosis,
	})
	if err != nil {
		// Don't roll back the cordon — Slack is best-effort.
		r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
			"BuildApplied: %v", err)
	} else {
		res := r.Slack.Post(ctx, payload)
		if res.Posted {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindApplied, metrics.SlackResultOK).Inc()
		} else {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindApplied, metrics.SlackResultErr).Inc()
			r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
				"slack post failed after %d attempts: %v", res.Attempts, res.LastErr)
		}
	}

	// 3. Status update — record action + transition to Acted.
	patchBase := nhd.DeepCopy()
	nhd.Status.Action = &nodemedicv1alpha1.ActionStatus{
		Decision:  nodemedicv1alpha1.ActionDecisionApplied,
		Operation: nodemedicv1alpha1.ActionOperationCordon,
		AppliedAt: ptrTime(now),
	}
	setCondition(&nhd.Status.Conditions, metav1.Condition{
		Type:               "ActionApplied",
		Status:             metav1.ConditionTrue,
		Reason:             "Cordoned",
		Message:            gate.Reason,
		LastTransitionTime: metav1.NewTime(now),
	})
	nhd.Status.Phase = nodemedicv1alpha1.PhaseActed
	if err := r.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("status patch: %w", err)
	}
	metrics.CasesTotal.WithLabelValues(metrics.OutcomeApplied).Inc()
	r.Recorder.Eventf(nhd, corev1.EventTypeNormal, "PhaseTransition",
		"%s -> Acted (Applied)", nodemedicv1alpha1.PhaseDiagnosed)
	return ctrl.Result{}, nil
}

func (r *NHDReconciler) humanInLoopPath(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	gate GateResult,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Slack first, then status. If Slack fails we still record the
	// HumanInLoop decision so the operator sees the case in
	// `kubectl get nhd`; the NotifierFailed Event is the secondary
	// signal that the side-channel notification didn't land.
	payload, buildErr := notifier.BuildHumanInLoop(notifier.HumanInLoopInput{
		NodeName:    nhd.Spec.Case.NodeName,
		ClusterName: nhd.Spec.Case.ClusterName,
		Namespace:   nhd.Namespace,
		NHDName:     nhd.Name,
		Diagnosis:   nhd.Status.Diagnosis,
		GateReason:  gate.Reason,
	})
	if buildErr != nil {
		r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
			"BuildHumanInLoop: %v", buildErr)
	} else {
		res := r.Slack.Post(ctx, payload)
		if res.Posted {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindHumanInLoop, metrics.SlackResultOK).Inc()
		} else {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindHumanInLoop, metrics.SlackResultErr).Inc()
			r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
				"slack post (HumanInLoop) failed after %d attempts: %v", res.Attempts, res.LastErr)
		}
	}
	logger.Info("gate-fail HumanInLoop", "node", nhd.Spec.Case.NodeName, "reason", gate.Reason)

	now := r.now()
	patchBase := nhd.DeepCopy()
	nhd.Status.Action = &nodemedicv1alpha1.ActionStatus{
		Decision:  nodemedicv1alpha1.ActionDecisionHumanInLoop,
		Operation: nodemedicv1alpha1.ActionOperationNoop,
		AppliedAt: ptrTime(now),
	}
	setCondition(&nhd.Status.Conditions, metav1.Condition{
		Type:               "ActionApplied",
		Status:             metav1.ConditionTrue,
		Reason:             "HumanInLoop",
		Message:            gate.Reason,
		LastTransitionTime: metav1.NewTime(now),
	})
	nhd.Status.Phase = nodemedicv1alpha1.PhaseActed
	if err := r.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("status patch: %w", err)
	}
	metrics.CasesTotal.WithLabelValues(metrics.OutcomeHumanInLoop).Inc()
	r.Recorder.Eventf(nhd, corev1.EventTypeNormal, "PhaseTransition",
		"%s -> Acted (HumanInLoop)", nodemedicv1alpha1.PhaseDiagnosed)
	return ctrl.Result{}, nil
}

// handleDeadlineExceeded implements FR-7 deadline retry-once. First
// hit: bump retry-count, push spec.budgets.deadline forward, re-issue
// POST /diagnose. Second hit (retry-count >= 1): terminal Critical.
func (r *NHDReconciler) handleDeadlineExceeded(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
) (ctrl.Result, error) {
	if getRetryCount(nhd) >= 1 {
		var deadlineStr string
		if d := nhd.Spec.Budgets.Deadline; d != nil {
			deadlineStr = d.Time.UTC().Format(time.RFC3339)
		}
		return r.criticalFailure(ctx, nhd, "DeadlineExceeded",
			fmt.Sprintf("agent did not write phase=Diagnosed before retried deadline %s", deadlineStr))
	}

	// First miss: bump annotation + push deadline forward.
	if err := r.bumpRetryAnnotation(ctx, nhd, 1); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.extendDeadline(ctx, nhd); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "RetryTriggered",
		"DeadlineExceeded; retrying POST /diagnose with same caseId=%s", nhd.Spec.Case.CaseId)

	// Re-issue. invokeAgent will write AgentInvoked condition and put
	// us back in Diagnosing — or escalate if even the retry fails.
	return r.invokeAgent(ctx, log.FromContext(ctx).WithName("retry"), nhd)
}

// handleAgentUnreachable implements FR-7 AgentUnreachable retry-once.
// Bumps retry-count immediately and asks the workqueue to requeue;
// on the next reconcile the agent client gets called again. If it
// fails a second time we land here with retry-count==1 and go
// terminal Critical.
func (r *NHDReconciler) handleAgentUnreachable(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	attErr *agentclient.AttemptError,
) (ctrl.Result, error) {
	if getRetryCount(nhd) >= 1 {
		return r.criticalFailure(ctx, nhd, "AgentUnreachable", attErr.Error())
	}
	if err := r.bumpRetryAnnotation(ctx, nhd, 1); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "RetryTriggered",
		"AgentUnreachable (%s); retrying POST /diagnose", attErr.Class)
	return ctrl.Result{Requeue: true}, nil
}

// extendDeadline patches spec.budgets.deadline forward by
// RetryDeadlineExtension (default 60s). Spec mutation is bounded to
// one call per case per FR-7's retry-once cap.
func (r *NHDReconciler) extendDeadline(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
) error {
	ext := r.RetryDeadlineExtension
	if ext <= 0 {
		ext = retryDeadlineExtensionDefault
	}
	patchBase := nhd.DeepCopy()
	newDeadline := metav1.NewTime(r.now().Add(ext))
	nhd.Spec.Budgets.Deadline = &newDeadline
	return r.Patch(ctx, nhd, client.MergeFrom(patchBase))
}

// bumpRetryAnnotation patches metadata.annotations to record the new
// retry-count. Idempotent; if the annotation already equals `to` it's
// a no-op patch (controller-runtime skips empty patches).
func (r *NHDReconciler) bumpRetryAnnotation(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	to int,
) error {
	patchBase := nhd.DeepCopy()
	if nhd.Annotations == nil {
		nhd.Annotations = map[string]string{}
	}
	nhd.Annotations[RetryCountAnnotation] = strconv.Itoa(to)
	return r.Patch(ctx, nhd, client.MergeFrom(patchBase))
}

// criticalFailure marks the NHD terminally Failed and posts a
// Critical Slack message. Best-effort on the Slack post; status is
// always written so the case is auditable in `kubectl get nhd`.
func (r *NHDReconciler) criticalFailure(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	reason, detail string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	attempts := getRetryCount(nhd) + 1 // burn-count +1 for the final
	payload, buildErr := notifier.BuildCritical(notifier.CriticalInput{
		NodeName:      nhd.Spec.Case.NodeName,
		ClusterName:   nhd.Spec.Case.ClusterName,
		Namespace:     nhd.Namespace,
		NHDName:       nhd.Name,
		FailureReason: reason,
		FailureDetail: detail,
		Attempts:      attempts,
	})
	if buildErr != nil {
		r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
			"BuildCritical: %v", buildErr)
	} else {
		res := r.Slack.Post(ctx, payload)
		if res.Posted {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindCritical, metrics.SlackResultOK).Inc()
		} else {
			metrics.SlackPostTotal.WithLabelValues(metrics.SlackKindCritical, metrics.SlackResultErr).Inc()
			r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "NotifierFailed",
				"slack post (Critical) failed after %d attempts: %v", res.Attempts, res.LastErr)
		}
	}
	logger.Error(errors.New(reason), "terminal Critical", "detail", detail)

	return r.markFailed(ctx, nhd, reason, detail)
}

func (r *NHDReconciler) markFailed(ctx context.Context, nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI, reason, message string) (ctrl.Result, error) {
	now := r.now()
	patchBase := nhd.DeepCopy()
	nhd.Status.Phase = nodemedicv1alpha1.PhaseFailed
	setCondition(&nhd.Status.Conditions, metav1.Condition{
		Type:               "AgentInvoked",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(now),
	})
	if err := r.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("status patch (Failed): %w", err)
	}
	metrics.CasesTotal.WithLabelValues(metrics.OutcomeFailed).Inc()
	r.Recorder.Eventf(nhd, corev1.EventTypeWarning, "PhaseTransition", "-> Failed: %s", reason)
	return ctrl.Result{}, nil
}

// transitionPhase patches phase + appends a Condition. Idempotent if
// the phase is already at `to`.
func (r *NHDReconciler) transitionPhase(
	ctx context.Context,
	nhd *nodemedicv1alpha1.NodeHealthDiagnosisAI,
	to nodemedicv1alpha1.Phase,
	cond metav1.Condition,
) error {
	if nhd.Status.Phase == to {
		return nil
	}
	patchBase := nhd.DeepCopy()
	nhd.Status.Phase = to
	cond.LastTransitionTime = metav1.NewTime(r.now())
	setCondition(&nhd.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, nhd, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("transition to %s: %w", to, err)
	}
	r.Recorder.Eventf(nhd, corev1.EventTypeNormal, "PhaseTransition", "-> %s", to)
	return nil
}

func (r *NHDReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// setCondition merges the new condition into conds in place, replacing
// any existing entry with the same Type. Standard k8s Conditions
// patch semantics.
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i, existing := range *conds {
		if existing.Type == c.Type {
			// Preserve LastTransitionTime if status didn't actually
			// change — that matches the standard meta/v1 helper.
			if existing.Status == c.Status && existing.Reason == c.Reason {
				return
			}
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}
