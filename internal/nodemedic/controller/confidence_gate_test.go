/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// helper builders keep the table cases short.
func evidence(sources ...nodemedicv1alpha1.EvidenceSource) []nodemedicv1alpha1.EvidenceItem {
	out := make([]nodemedicv1alpha1.EvidenceItem, 0, len(sources))
	for _, s := range sources {
		out = append(out, nodemedicv1alpha1.EvidenceItem{Source: s})
	}
	return out
}

func diag(conf float64, sources []nodemedicv1alpha1.EvidenceItem, action nodemedicv1alpha1.RecommendationAction) *nodemedicv1alpha1.Diagnosis {
	return &nodemedicv1alpha1.Diagnosis{
		Confidence: conf,
		Evidence:   sources,
		Recommendation: &nodemedicv1alpha1.Recommendation{
			Action: action,
		},
	}
}

func TestConfidenceGate(t *testing.T) {
	t.Parallel()

	const (
		minConf    = 0.7
		minSources = 2
	)

	cases := []struct {
		name       string
		diagnosis  *nodemedicv1alpha1.Diagnosis
		want       GateOutcome
		wantSubstr string // substring expected in Reason
	}{
		{
			name: "all clauses pass — Cordon",
			diagnosis: diag(0.85,
				evidence("nrql", "ssh", "kubectl"),
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want:       GatePass,
			wantSubstr: "confidence 0.85 >= 0.70",
		},
		{
			name: "all clauses pass — DrainAndCordon",
			diagnosis: diag(0.95,
				evidence("nrql", "cloud"),
				nodemedicv1alpha1.RecommendationActionDrainAndCordon,
			),
			want:       GatePass,
			wantSubstr: "DrainAndCordon",
		},
		{
			name: "boundary: confidence exactly 0.7",
			diagnosis: diag(0.70,
				evidence("nrql", "ssh"),
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want: GatePass,
		},
		{
			name: "fail: confidence below threshold",
			diagnosis: diag(0.5,
				evidence("nrql", "ssh", "kubectl"),
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want:       GateFail,
			wantSubstr: "confidence 0.50 < min 0.70",
		},
		{
			name: "fail: only one distinct source (counts unique, not entries)",
			diagnosis: diag(0.85,
				evidence("nrql", "nrql", "nrql"), // 3 entries, 1 source
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want:       GateFail,
			wantSubstr: "distinct sources 1 < min 2",
		},
		{
			name: "fail: zero evidence",
			diagnosis: diag(0.85,
				nil,
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want:       GateFail,
			wantSubstr: "distinct sources 0 < min 2",
		},
		{
			name: "fail: missing recommendation",
			diagnosis: &nodemedicv1alpha1.Diagnosis{
				Confidence: 0.85,
				Evidence:   evidence("nrql", "ssh"),
			},
			want:       GateFail,
			wantSubstr: "missing recommendation",
		},
		{
			name: "fail: unknown action",
			diagnosis: diag(0.85,
				evidence("nrql", "ssh"),
				nodemedicv1alpha1.RecommendationAction("Reboot"),
			),
			want:       GateFail,
			wantSubstr: `not in {Cordon, DrainAndCordon}`,
		},
		{
			name: "skip: NoAction (separate from fail)",
			diagnosis: diag(0.95,
				evidence("nrql", "ssh", "kubectl"),
				nodemedicv1alpha1.RecommendationActionNoAction,
			),
			want:       GateSkip,
			wantSubstr: "NoAction",
		},
		{
			name:       "fail: nil diagnosis",
			diagnosis:  nil,
			want:       GateFail,
			wantSubstr: "no diagnosis",
		},
		{
			name: "fail: cumulates all reasons",
			diagnosis: diag(0.5,
				evidence("nrql"), // 1 source < 2
				nodemedicv1alpha1.RecommendationAction("NoAction"),
			),
			// NoAction takes the Skip branch and short-circuits the
			// other failures — that's intentional.
			want:       GateSkip,
			wantSubstr: "NoAction",
		},
		{
			name: "fail: cumulates confidence+sources",
			diagnosis: diag(0.5,
				evidence("nrql"),
				nodemedicv1alpha1.RecommendationActionCordon,
			),
			want:       GateFail,
			wantSubstr: "confidence 0.50 < min 0.70; distinct sources 1 < min 2",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ConfidenceGate(tc.diagnosis, minConf, minSources)
			if got.Outcome != tc.want {
				t.Errorf("Outcome = %v, want %v (reason=%q)", got.Outcome, tc.want, got.Reason)
			}
			if tc.wantSubstr != "" && !strings.Contains(got.Reason, tc.wantSubstr) {
				t.Errorf("Reason %q does not contain %q", got.Reason, tc.wantSubstr)
			}
		})
	}
}

func TestConfidenceGate_DistinctSourcesSorted(t *testing.T) {
	t.Parallel()
	d := diag(0.85,
		evidence("ssh", "nrql", "kubectl", "nrql", "cloud"),
		nodemedicv1alpha1.RecommendationActionCordon,
	)
	got := ConfidenceGate(d, 0.7, 2)
	want := []string{"cloud", "kubectl", "nrql", "ssh"}
	if len(got.SourcesByName) != len(want) {
		t.Fatalf("got %v, want %v", got.SourcesByName, want)
	}
	for i, s := range want {
		if got.SourcesByName[i] != s {
			t.Errorf("SourcesByName[%d] = %q, want %q", i, got.SourcesByName[i], s)
		}
	}
}
