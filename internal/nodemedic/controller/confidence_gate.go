/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"sort"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// ConfidenceGate is the binding rule from Constitution Article I.3 /
// spec FR-6. Auto-cordon iff ALL three clauses hold:
//
//  1. confidence >= minConfidence
//  2. number of distinct evidence sources >= minSources
//  3. recommendation.action ∈ {Cordon, DrainAndCordon}
//
// recommendation.action == NoAction takes the GateSkip path: do not
// cordon, route to HumanInLoop framing. Anything else (missing
// diagnosis, malformed action) returns GateFail.
//
// This function is pure: no I/O, no clock, no global state. Tests
// exercise each clause independently.
func ConfidenceGate(d *nodemedicv1alpha1.Diagnosis, minConfidence float64, minSources int) GateResult {
	if d == nil {
		return GateResult{
			Outcome: GateFail,
			Reason:  "no diagnosis written",
		}
	}

	sources := distinctSources(d.Evidence)
	res := GateResult{
		Confidence:      d.Confidence,
		DistinctSources: len(sources),
		SourcesByName:   sources,
	}
	if d.Recommendation != nil {
		res.RecommendedAction = string(d.Recommendation.Action)
	}

	// NoAction is its own bucket — neither pass nor fail. The agent
	// explicitly recommended doing nothing; the controller honors that
	// without cordoning, but the operator still wants a human-in-loop
	// notification per spec.
	if d.Recommendation != nil && d.Recommendation.Action == nodemedicv1alpha1.RecommendationActionNoAction {
		res.Outcome = GateSkip
		res.Reason = "recommendation.action=NoAction"
		return res
	}

	// Collect every failing clause so the Reason names ALL the reasons,
	// not just the first one. Aids the operator's Slack triage.
	var fails []string
	if d.Confidence < minConfidence {
		fails = append(fails, fmt.Sprintf("confidence %.2f < min %.2f", d.Confidence, minConfidence))
	}
	if res.DistinctSources < minSources {
		fails = append(fails, fmt.Sprintf("distinct sources %d < min %d", res.DistinctSources, minSources))
	}
	if d.Recommendation == nil {
		fails = append(fails, "missing recommendation")
	} else if !isCordonAction(d.Recommendation.Action) {
		fails = append(fails, fmt.Sprintf("recommendation.action=%q not in {Cordon, DrainAndCordon}", d.Recommendation.Action))
	}

	if len(fails) == 0 {
		res.Outcome = GatePass
		res.Reason = fmt.Sprintf("confidence %.2f >= %.2f, %d distinct sources >= %d, action=%s",
			d.Confidence, minConfidence, res.DistinctSources, minSources, d.Recommendation.Action)
		return res
	}

	res.Outcome = GateFail
	res.Reason = "gate failed: " + joinReasons(fails)
	return res
}

func isCordonAction(a nodemedicv1alpha1.RecommendationAction) bool {
	return a == nodemedicv1alpha1.RecommendationActionCordon ||
		a == nodemedicv1alpha1.RecommendationActionDrainAndCordon
}

// distinctSources returns the unique evidence-source names in
// alphabetical order. Used for the count check and for surfacing
// which sources fired in the Slack message.
func distinctSources(items []nodemedicv1alpha1.EvidenceItem) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it.Source == "" {
			continue
		}
		seen[string(it.Source)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func joinReasons(reasons []string) string {
	switch len(reasons) {
	case 0:
		return ""
	case 1:
		return reasons[0]
	}
	out := reasons[0]
	for _, r := range reasons[1:] {
		out += "; " + r
	}
	return out
}
