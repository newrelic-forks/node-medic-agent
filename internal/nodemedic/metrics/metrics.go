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

// Package metrics registers the Prometheus collectors enumerated in spec
// NFR-3 against the controller-runtime manager's built-in metrics
// registry (sigs.k8s.io/controller-runtime/pkg/metrics).
//
// Collectors:
//
//	nodemedic_cases_total{outcome="Applied|HumanInLoop|Failed"}
//	nodemedic_phase_duration_seconds{phase="Pending|Diagnosing|Diagnosed|Acted"}
//	nodemedic_agent_post_total{result="ok|429|5xx|timeout"}
//	nodemedic_cordon_total{result="ok|err"}
//	nodemedic_slack_post_total{kind="Applied|HumanInLoop|Critical",result="ok|err"}
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// OutcomeApplied is the `outcome` label value when the controller
	// cordoned a Node after a passing gate.
	OutcomeApplied = "Applied"
	// OutcomeHumanInLoop is the label for the gate-fail path.
	OutcomeHumanInLoop = "HumanInLoop"
	// OutcomeFailed is the label for terminal Failed cases (agent timeout,
	// 4xx/5xx, etc.).
	OutcomeFailed = "Failed"

	// PhasePending et al. are the `phase` label values for
	// nodemedic_phase_duration_seconds.
	PhasePending    = "Pending"
	PhaseDiagnosing = "Diagnosing"
	PhaseDiagnosed  = "Diagnosed"
	PhaseActed      = "Acted"

	// AgentPostOK et al. are the `result` label values for
	// nodemedic_agent_post_total.
	AgentPostOK      = "ok"
	AgentPostRetry   = "429"
	AgentPostServer  = "5xx"
	AgentPostTimeout = "timeout"

	// CordonOK / CordonErr label values.
	CordonOK  = "ok"
	CordonErr = "err"

	// SlackKindApplied et al. are the `kind` label values for
	// nodemedic_slack_post_total.
	SlackKindApplied     = "Applied"
	SlackKindHumanInLoop = "HumanInLoop"
	SlackKindCritical    = "Critical"

	// SlackResultOK / SlackResultErr label values.
	SlackResultOK  = "ok"
	SlackResultErr = "err"
)

var (
	// CasesTotal counts terminal-state cases by outcome.
	CasesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nodemedic_cases_total",
			Help: "Number of NodeHealthDiagnosisAI cases that reached a terminal phase, by outcome.",
		},
		[]string{"outcome"},
	)

	// PhaseDurationSeconds tracks how long each phase took, end to end.
	PhaseDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nodemedic_phase_duration_seconds",
			Help:    "Wall-clock duration of each NodeHealthDiagnosisAI phase, in seconds.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		},
		[]string{"phase"},
	)

	// AgentPostTotal counts POST /diagnose attempts by result class.
	AgentPostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nodemedic_agent_post_total",
			Help: "Number of POST /diagnose attempts to the agent service, by result class.",
		},
		[]string{"result"},
	)

	// CordonTotal counts cordon-patch attempts by result.
	CordonTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nodemedic_cordon_total",
			Help: "Number of cordon patches issued against Node.spec.unschedulable, by result.",
		},
		[]string{"result"},
	)

	// SlackPostTotal counts Slack notification attempts by kind and result.
	SlackPostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nodemedic_slack_post_total",
			Help: "Number of Slack notification attempts, by message kind and result.",
		},
		[]string{"kind", "result"},
	)
)

// MustRegister installs all collectors into the controller-runtime
// metrics registry. Call once from main.go before ctrl.NewManager.Start
// returns. Panics if any collector is already registered (which would
// indicate a duplicate Init call — desirable to fail fast at startup).
func MustRegister() {
	ctrlmetrics.Registry.MustRegister(
		CasesTotal,
		PhaseDurationSeconds,
		AgentPostTotal,
		CordonTotal,
		SlackPostTotal,
	)
}
