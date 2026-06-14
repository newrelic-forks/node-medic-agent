/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package notifier

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// T025 — golden coverage of the three Block Kit builders against the
// frozen JSON shapes in
// .specify/specs/003-nodemedic-oncall-ui/contracts/slack-block-kit.md.
//
// We compare the parsed-JSON tree of (build → marshal) against the
// parsed-JSON tree of the golden file so a different key order in the
// builder implementation does not falsely break the test. Trailing
// whitespace and field ordering are intentionally ignored; the byte
// shape for the wire is locked elsewhere by the contract doc.

const goldenDir = "testdata/block-kit"

// loadGolden reads testdata/block-kit/<name>.json and decodes it into
// an interface{} tree for cmp.Diff against the builder output.
func loadGolden(t *testing.T, name string) any {
	t.Helper()
	p := filepath.Join(goldenDir, name+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v", p, err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("parse golden %s: %v", p, err)
	}
	return tree
}

// parseBuilder decodes the builder's []byte return into an interface{}
// tree so cmp.Diff compares semantic JSON content, not byte ordering.
func parseBuilder(t *testing.T, b []byte) any {
	t.Helper()
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("parse builder output: %v\nraw: %s", err, string(b))
	}
	return tree
}

// goldenAppliedBlockKitInput mirrors §1 of the contract doc:
//   - node:    cf1z-general-nodes-1000007
//   - cluster: cf1z
//   - confidence: 0.92
//   - rcaCategory: Kubelet
//   - trigger: KubeletUnhealthy
//   - nhd:    cf1z-general-nodes-1000007-1718374200
//   - uiBaseURL: http://localhost:8080
func goldenAppliedBlockKitInput() BlockKitInput {
	return BlockKitInput{
		NodeName:    "cf1z-general-nodes-1000007",
		ClusterName: "cf1z",
		Namespace:   "cf-monitoring",
		NHDName:     "cf1z-general-nodes-1000007-1718374200",
		UIBaseURL:   "http://localhost:8080",
		Trigger:     "KubeletUnhealthy",
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RCACategory: nodemedicv1alpha1.RCACategory("Kubelet"),
			Confidence:  0.92,
		},
	}
}

func goldenHumanInLoopBlockKitInput() BlockKitInput {
	return BlockKitInput{
		NodeName:    "cf1z-general-nodes-2000007",
		ClusterName: "cf1z",
		Namespace:   "cf-monitoring",
		NHDName:     "cf1z-general-nodes-2000007-1718374500",
		UIBaseURL:   "http://localhost:8080",
		Trigger:     "KubeletUnhealthy",
		Diagnosis: &nodemedicv1alpha1.Diagnosis{
			RCACategory: nodemedicv1alpha1.RCACategory("Kubelet"),
			Confidence:  0.42,
		},
	}
}

func goldenFailedBlockKitInput() BlockKitInput {
	return BlockKitInput{
		NodeName:    "cf1z-general-nodes-3000007",
		ClusterName: "cf1z",
		Namespace:   "cf-monitoring",
		NHDName:     "cf1z-general-nodes-3000007-1718375100",
		UIBaseURL:   "http://localhost:8080",
		Trigger:     "KubeletUnhealthy",
		Failure: &FailureView{
			Reason:   "AgentUnreachable",
			Attempts: 2,
		},
	}
}

func TestBuildBlockKitApplied_Golden(t *testing.T) {
	t.Parallel()

	got, err := BuildBlockKitApplied(goldenAppliedBlockKitInput())
	if err != nil {
		t.Fatalf("BuildBlockKitApplied: %v", err)
	}
	want := loadGolden(t, "applied")
	if diff := cmp.Diff(want, parseBuilder(t, got)); diff != "" {
		t.Fatalf("BuildBlockKitApplied output diverges from contracts/slack-block-kit.md §1:\n%s", diff)
	}
}

func TestBuildBlockKitHumanInLoop_Golden(t *testing.T) {
	t.Parallel()

	got, err := BuildBlockKitHumanInLoop(goldenHumanInLoopBlockKitInput())
	if err != nil {
		t.Fatalf("BuildBlockKitHumanInLoop: %v", err)
	}
	want := loadGolden(t, "human-in-loop")
	if diff := cmp.Diff(want, parseBuilder(t, got)); diff != "" {
		t.Fatalf("BuildBlockKitHumanInLoop output diverges from contracts/slack-block-kit.md §2:\n%s", diff)
	}
}

func TestBuildBlockKitFailed_Golden(t *testing.T) {
	t.Parallel()

	got, err := BuildBlockKitFailed(goldenFailedBlockKitInput())
	if err != nil {
		t.Fatalf("BuildBlockKitFailed: %v", err)
	}
	want := loadGolden(t, "failed")
	if diff := cmp.Diff(want, parseBuilder(t, got)); diff != "" {
		t.Fatalf("BuildBlockKitFailed output diverges from contracts/slack-block-kit.md §3:\n%s", diff)
	}
}

// Negative coverage — empty UIBaseURL is a hard error for all three
// builders since the "View full diagnosis" button URL is non-optional.
func TestBuildBlockKit_EmptyUIBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		build func(in BlockKitInput) ([]byte, error)
		in    BlockKitInput
	}{
		{"applied", BuildBlockKitApplied, func() BlockKitInput {
			in := goldenAppliedBlockKitInput()
			in.UIBaseURL = ""
			return in
		}()},
		{"human-in-loop", BuildBlockKitHumanInLoop, func() BlockKitInput {
			in := goldenHumanInLoopBlockKitInput()
			in.UIBaseURL = ""
			return in
		}()},
		{"failed", BuildBlockKitFailed, func() BlockKitInput {
			in := goldenFailedBlockKitInput()
			in.UIBaseURL = ""
			return in
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.build(tc.in)
			if err == nil {
				t.Fatalf("expected error for empty UIBaseURL, got payload: %s", string(b))
			}
		})
	}
}

// Applied + HumanInLoop reject nil diagnosis (the field source mapping
// in §1 / §2 reads Confidence + RCACategory from it). Failed accepts
// nil diagnosis — that's the normal shape of a failed case.
func TestBuildBlockKit_NilDiagnosis(t *testing.T) {
	t.Parallel()

	t.Run("applied rejects nil diagnosis", func(t *testing.T) {
		in := goldenAppliedBlockKitInput()
		in.Diagnosis = nil
		if _, err := BuildBlockKitApplied(in); err == nil {
			t.Fatal("expected error for nil diagnosis on Applied")
		}
	})

	t.Run("human-in-loop rejects nil diagnosis", func(t *testing.T) {
		in := goldenHumanInLoopBlockKitInput()
		in.Diagnosis = nil
		if _, err := BuildBlockKitHumanInLoop(in); err == nil {
			t.Fatal("expected error for nil diagnosis on HumanInLoop")
		}
	})

	t.Run("failed accepts nil diagnosis", func(t *testing.T) {
		in := goldenFailedBlockKitInput()
		in.Diagnosis = nil
		if _, err := BuildBlockKitFailed(in); err != nil {
			t.Fatalf("BuildBlockKitFailed with nil diagnosis returned error: %v", err)
		}
	})
}

// Failed builder requires a populated Failure pointer — the message
// needs the reason + attempts count for the field grid.
func TestBuildBlockKitFailed_RequiresFailure(t *testing.T) {
	t.Parallel()

	in := goldenFailedBlockKitInput()
	in.Failure = nil
	if _, err := BuildBlockKitFailed(in); err == nil {
		t.Fatal("expected error for nil Failure on Failed builder")
	}
}

// Documents that the URL is emitted as-is — no path encoding. Spec 001
// names use only [a-z0-9-] which is path-safe by construction. If the
// name format ever grows URL-unsafe characters, this test will catch
// the silent regression that the builder didn't follow.
func TestBuildBlockKitApplied_NHDNameNotEncoded(t *testing.T) {
	t.Parallel()

	in := goldenAppliedBlockKitInput()
	in.NHDName = "node-with-no-special-chars-1000"
	got, err := BuildBlockKitApplied(in)
	if err != nil {
		t.Fatalf("BuildBlockKitApplied: %v", err)
	}
	wantURL := "http://localhost:8080/cases/node-with-no-special-chars-1000"
	if !strings.Contains(string(got), wantURL) {
		t.Fatalf("payload missing expected URL %q\npayload: %s", wantURL, string(got))
	}
}

// Severity matrix sanity — color comes from the builder, not the input.
func TestBuildBlockKit_SeverityColors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		build     func(in BlockKitInput) ([]byte, error)
		input     BlockKitInput
		wantColor string
	}{
		{"applied → danger", BuildBlockKitApplied, goldenAppliedBlockKitInput(), "danger"},
		{"human-in-loop → warning", BuildBlockKitHumanInLoop, goldenHumanInLoopBlockKitInput(), "warning"},
		{"failed → #808080", BuildBlockKitFailed, goldenFailedBlockKitInput(), "#808080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.build(tc.input)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var tree map[string]any
			if err := json.Unmarshal(raw, &tree); err != nil {
				t.Fatalf("parse: %v", err)
			}
			atts, ok := tree["attachments"].([]any)
			if !ok || len(atts) != 1 {
				t.Fatalf("expected exactly one attachment, got: %v", tree["attachments"])
			}
			att, _ := atts[0].(map[string]any)
			if got := att["color"]; got != tc.wantColor {
				t.Fatalf("attachment.color = %v, want %q", got, tc.wantColor)
			}
		})
	}
}
