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
	"fmt"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/oncall/slack"
)

// BlockKitInput is the data needed by the three Block Kit builders.
// Same shape as AppliedInput / HumanInLoopInput / CriticalInput plus
// UIBaseURL (for the "View full diagnosis" button) and the per-case
// Trigger string (carried from nhd.spec.case.trigger.type).
//
// Field source mapping is locked by
// .specify/specs/003-nodemedic-oncall-ui/contracts/slack-block-kit.md.
type BlockKitInput struct {
	NodeName    string
	ClusterName string
	Namespace   string // CR namespace (cf-monitoring)
	NHDName     string // metadata.name of the NHD
	UIBaseURL   string // e.g. "http://localhost:8080" — required
	Trigger     string // nhd.spec.case.trigger.type, e.g. "KubeletUnhealthy"
	Diagnosis   *nodemedicv1alpha1.Diagnosis
	GateReason  string       // populated only for HumanInLoop; reserved for future
	Failure     *FailureView // populated only for Failed
}

// FailureView carries the Failed-builder fields the controller derives
// from the NHD on a terminal-Failed transition (spec FR-7 retry burned
// or ResultBadRequest etc.).
type FailureView struct {
	Reason   string // "DeadlineExceeded" | "AgentUnreachable" | "BadRequest" | ...
	Detail   string // currently unused in Slack body; reserved for future
	Attempts int
}

// blockKitEnvelope mirrors the contract's top-level shape: text +
// blocks[] + attachments[]. Field tags are explicit so JSON marshaling
// is byte-stable across Go versions.
type blockKitEnvelope struct {
	Text        string               `json:"text"`
	Blocks      []blockKitBlock      `json:"blocks"`
	Attachments []blockKitAttachment `json:"attachments"`
}

type blockKitBlock struct {
	Type     string           `json:"type"`
	Text     *blockKitText    `json:"text,omitempty"`
	Fields   []blockKitText   `json:"fields,omitempty"`
	Elements []blockKitButton `json:"elements,omitempty"`
}

type blockKitText struct {
	Type string `json:"type"` // "plain_text" | "mrkdwn"
	Text string `json:"text"`
}

type blockKitButton struct {
	Type  string        `json:"type"` // "button"
	Style string        `json:"style,omitempty"`
	Text  *blockKitText `json:"text"`
	URL   string        `json:"url"`
}

type blockKitAttachment struct {
	Color    string `json:"color"`
	Fallback string `json:"fallback"`
}

// fieldText returns a `mrkdwn` field with the contract's `*Label:*\nValue` shape.
func fieldText(label, value string) blockKitText {
	return blockKitText{
		Type: "mrkdwn",
		Text: fmt.Sprintf("*%s:*\n%s", label, value),
	}
}

// viewButton returns the single primary "View full diagnosis" action
// element pointing at <uiBaseURL>/cases/<nhdName>. Caller is
// responsible for surfacing any URL-build error.
func viewButton(url string) []blockKitButton {
	return []blockKitButton{{
		Type:  "button",
		Style: "primary",
		Text:  &blockKitText{Type: "plain_text", Text: "View full diagnosis"},
		URL:   url,
	}}
}

// BuildBlockKitApplied returns the Slack JSON for a gate-pass +
// cordon-applied case. Header is "🚨 Cordoned: <node> (<cluster>)";
// color is `danger`; field grid carries Decision/Confidence/Trigger/rcaCategory.
// See contracts/slack-block-kit.md §1.
func BuildBlockKitApplied(in BlockKitInput) ([]byte, error) {
	if in.Diagnosis == nil {
		return nil, fmt.Errorf("BuildBlockKitApplied: nil Diagnosis")
	}
	caseURL, err := slack.BuildCaseURL(in.UIBaseURL, in.NHDName)
	if err != nil {
		return nil, fmt.Errorf("BuildBlockKitApplied: %w", err)
	}
	d := in.Diagnosis

	rca := string(d.RCACategory)
	if rca == "" {
		rca = "Unknown"
	}
	confidence := fmt.Sprintf("%.2f", d.Confidence)
	header := fmt.Sprintf("🚨 Cordoned: %s (%s)", in.NodeName, in.ClusterName)
	fallback := fmt.Sprintf("Cordoned %s in %s: Applied @ %s — View full diagnosis: %s",
		in.NodeName, in.ClusterName, confidence, caseURL)

	env := blockKitEnvelope{
		Text: fallback,
		Blocks: []blockKitBlock{
			{Type: "header", Text: &blockKitText{Type: "plain_text", Text: header}},
			{Type: "section", Fields: []blockKitText{
				fieldText("Decision", "Applied"),
				fieldText("Confidence", confidence),
				fieldText("Trigger", in.Trigger),
				fieldText("rcaCategory", rca),
			}},
			{Type: "actions", Elements: viewButton(caseURL)},
		},
		Attachments: []blockKitAttachment{
			{Color: "danger", Fallback: fallback},
		},
	}
	return json.Marshal(env)
}

// BuildBlockKitHumanInLoop returns the Slack JSON for a gate-fail
// case. Header is "⚠️ Needs review: <node> (<cluster>)"; color is
// `warning`; same 4-field grid as Applied. Node is NOT cordoned.
// See contracts/slack-block-kit.md §2.
func BuildBlockKitHumanInLoop(in BlockKitInput) ([]byte, error) {
	if in.Diagnosis == nil {
		return nil, fmt.Errorf("BuildBlockKitHumanInLoop: nil Diagnosis")
	}
	caseURL, err := slack.BuildCaseURL(in.UIBaseURL, in.NHDName)
	if err != nil {
		return nil, fmt.Errorf("BuildBlockKitHumanInLoop: %w", err)
	}
	d := in.Diagnosis

	rca := string(d.RCACategory)
	if rca == "" {
		rca = "Unknown"
	}
	confidence := fmt.Sprintf("%.2f", d.Confidence)
	header := fmt.Sprintf("⚠️ Needs review: %s (%s)", in.NodeName, in.ClusterName)
	fallback := fmt.Sprintf("Needs review on %s in %s: HumanInLoop @ %s — View full diagnosis: %s",
		in.NodeName, in.ClusterName, confidence, caseURL)

	env := blockKitEnvelope{
		Text: fallback,
		Blocks: []blockKitBlock{
			{Type: "header", Text: &blockKitText{Type: "plain_text", Text: header}},
			{Type: "section", Fields: []blockKitText{
				fieldText("Decision", "HumanInLoop"),
				fieldText("Confidence", confidence),
				fieldText("Trigger", in.Trigger),
				fieldText("rcaCategory", rca),
			}},
			{Type: "actions", Elements: viewButton(caseURL)},
		},
		Attachments: []blockKitAttachment{
			{Color: "warning", Fallback: fallback},
		},
	}
	return json.Marshal(env)
}

// BuildBlockKitFailed returns the Slack JSON for a terminal-Failed
// case (FR-7 retry burned or non-retryable agent error). Header is
// "❌ Failed: <node> (<cluster>)"; color is hex `#808080`; field grid
// shows Decision (literal "(none — agent failed)") + Failure +
// Trigger + Attempts. Diagnosis may be nil; Failure must not be.
// See contracts/slack-block-kit.md §3.
func BuildBlockKitFailed(in BlockKitInput) ([]byte, error) {
	if in.Failure == nil {
		return nil, fmt.Errorf("BuildBlockKitFailed: nil Failure")
	}
	caseURL, err := slack.BuildCaseURL(in.UIBaseURL, in.NHDName)
	if err != nil {
		return nil, fmt.Errorf("BuildBlockKitFailed: %w", err)
	}

	reason := in.Failure.Reason
	if reason == "" {
		reason = "Failed"
	}
	header := fmt.Sprintf("❌ Failed: %s (%s)", in.NodeName, in.ClusterName)
	fallback := fmt.Sprintf("Failed on %s in %s: %s after %d attempts — View full diagnosis: %s",
		in.NodeName, in.ClusterName, reason, in.Failure.Attempts, caseURL)

	env := blockKitEnvelope{
		Text: fallback,
		Blocks: []blockKitBlock{
			{Type: "header", Text: &blockKitText{Type: "plain_text", Text: header}},
			{Type: "section", Fields: []blockKitText{
				fieldText("Decision", "(none — agent failed)"),
				fieldText("Failure", reason),
				fieldText("Trigger", in.Trigger),
				fieldText("Attempts", fmt.Sprintf("%d", in.Failure.Attempts)),
			}},
			{Type: "actions", Elements: viewButton(caseURL)},
		},
		Attachments: []blockKitAttachment{
			{Color: "#808080", Fallback: fallback},
		},
	}
	return json.Marshal(env)
}
