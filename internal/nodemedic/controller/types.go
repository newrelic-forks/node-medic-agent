/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller hosts the NodeMedic reconciler, Node watcher, case
// creator, confidence gate, cordon executor, debounce map, and NHD-name
// formatter.
//
// See:
//   - .specify/specs/001-nodemedic-controller/data-model.md §4 (Case)
//   - .specify/specs/001-nodemedic-controller/data-model.md §5 (GateResult)
//   - .specify/specs/001-nodemedic-controller/data-model.md §6 (DebounceMap)
package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// GateOutcome is the verdict produced by ConfidenceGate (FR-6).
type GateOutcome int

const (
	// GatePass means all three clauses of Constitution Article I.3 hold.
	GatePass GateOutcome = iota
	// GateFail means at least one clause failed.
	GateFail
	// GateSkip means recommendation.action == NoAction; controller does not
	// cordon and treats this like HumanInLoop framing.
	GateSkip
)

// String returns the lowercase name used in logs/metrics.
func (o GateOutcome) String() string {
	switch o {
	case GatePass:
		return "pass"
	case GateFail:
		return "fail"
	case GateSkip:
		return "skip"
	default:
		return "unknown"
	}
}

// GateResult is the structured output of the confidence gate.
type GateResult struct {
	Outcome           GateOutcome
	Confidence        float64
	DistinctSources   int
	SourcesByName     []string // sorted, deduped
	RecommendedAction string   // verbatim from NHD
	Reason            string   // human-readable; goes into Condition.message
}

// CordonResult is the outcome of patching Node.spec.unschedulable.
type CordonResult struct {
	Patched          bool   // false if already cordoned (idempotent no-op)
	Err              error  // non-nil if the patch failed
	Refused          bool   // true if Node.Name != NHD.Spec.Case.NodeName (FR-8)
}

// SlackResult is the outcome of a Slack post attempt chain.
type SlackResult struct {
	Posted   bool
	Attempts int
	LastErr  error
}

// Decisions accumulates everything the reconciler did during one
// Reconcile call. Each field is nil until the corresponding step runs.
type Decisions struct {
	GateResult       *GateResult
	CordonResult     *CordonResult
	SlackResult      *SlackResult
	NextRequeueAfter time.Duration // 0 = terminal
}

// Case is the in-process bundle for one Reconcile invocation. Not
// persisted; lives only inside the reconciler. Time is injected via Now
// so tests can drive deadlines deterministically.
type Case struct {
	Key       client.ObjectKey
	NHD       *nodemedicv1alpha1.NodeHealthDiagnosisAI
	Node      *corev1.Node // may be nil if the Node was deleted
	Now       time.Time
	Decisions Decisions
}

// DebounceKey identifies one (node, conditionType) pair for the debounce
// map (FR-1, data-model §6).
type DebounceKey struct {
	Node string
	Type string
}
