/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package notifier builds and posts Slack Block Kit messages for the
// terminal outcomes of an NHD case.
//
// Phase 3 (US1): BuildApplied — gate-pass + cordon.
// Phase 4 (US2): BuildHumanInLoop — gate-fail; node NOT cordoned.
// Phase 5 (US3): BuildCritical — agent failed terminally (after one
//                retry); node NOT cordoned, on-call paged.
package notifier

import (
	"encoding/json"
	"fmt"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
)

// AppliedInput is the data needed to build the `Applied` Slack
// message. The reconciler populates this from the NHD CR after a
// successful cordon.
type AppliedInput struct {
	NodeName    string
	ClusterName string
	Namespace   string // CR namespace (cf-monitoring), used for the kubectl link
	NHDName     string // metadata.name of the NHD
	Diagnosis   *nodemedicv1alpha1.Diagnosis
}

// HumanInLoopInput is the data needed to build the gate-failure
// Slack message. Mirrors AppliedInput but adds the gate's reason so
// the message tells the on-call WHY the gate didn't fire.
type HumanInLoopInput struct {
	NodeName    string
	ClusterName string
	Namespace   string
	NHDName     string
	Diagnosis   *nodemedicv1alpha1.Diagnosis // may be partial
	GateReason  string                       // pre-formatted from controller.GateResult.Reason
}

// CriticalInput drives BuildCritical. The agent never produced a
// usable diagnosis (timeout, 4xx, or two consecutive failures), so
// most fields are degenerate compared to Applied / HumanInLoop.
type CriticalInput struct {
	NodeName      string
	ClusterName   string
	Namespace     string
	NHDName       string
	FailureReason string // e.g. "DeadlineExceeded", "AgentUnreachable", "BadRequest"
	FailureDetail string // structured error message; surfaced verbatim
	Attempts      int    // 1 if first attempt failed terminally; 2 after the retry burned
}

// BuildApplied returns the Slack JSON payload for a gate-pass +
// cordon-applied case. Output shape matches data-model.md §8 and the
// scope.md §5.4 envelope; per research R-6 there is no "View CR"
// button — instead the message body includes a fenced kubectl block
// the operator can copy.
func BuildApplied(in AppliedInput) ([]byte, error) {
	if in.Diagnosis == nil {
		return nil, fmt.Errorf("BuildApplied: nil diagnosis")
	}
	d := in.Diagnosis

	header := fmt.Sprintf("NodeMedic: %s – %s (conf %.2f)",
		in.NodeName, fallback(string(d.RCACategory), "Unknown"), d.Confidence)

	rca := d.RootCause
	if rca == "" {
		rca = "(no root-cause text)"
	}

	kubectlBlock := fmt.Sprintf(
		"```\nkubectl --context=%s -n %s get nhd %s -o yaml\n```",
		in.ClusterName, fallback(in.Namespace, "cf-monitoring"), in.NHDName,
	)

	auditURL := ""
	if d.AuditLogRef != nil {
		auditURL = d.AuditLogRef.ObjectStore
	}

	msg := slackEnvelope{
		Text: header,
		Blocks: []slackBlock{
			{Type: "header", Text: &slackText{Type: "plain_text", Text: header}},
			{
				Type: "section",
				Fields: []slackText{
					{Type: "mrkdwn", Text: "*Cluster:* " + in.ClusterName},
					{Type: "mrkdwn", Text: "*Action:* cordoned"},
				},
			},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*RCA:* " + rca}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Inspect:*\n" + kubectlBlock}},
		},
	}

	if auditURL != "" {
		msg.Blocks = append(msg.Blocks, slackBlock{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: "*Audit log:* " + auditURL},
		})
	}

	return json.Marshal(msg)
}

// BuildHumanInLoop returns the Slack JSON payload for a gate-failure
// case. Spec FR-9: distinct framing from `Applied` (the operator must
// be able to tell at a glance that the node was NOT cordoned). We
// surface that with a different header verb ("needs human review"),
// an explicit Action field reading "NOT cordoned", and the gate's
// failure reason in its own block.
//
// Fields like RCA / kubectl-block / audit-log mirror the Applied
// shape so the on-call doesn't have to learn two layouts.
func BuildHumanInLoop(in HumanInLoopInput) ([]byte, error) {
	if in.Diagnosis == nil {
		return nil, fmt.Errorf("BuildHumanInLoop: nil diagnosis")
	}
	d := in.Diagnosis

	header := fmt.Sprintf("NodeMedic: %s – needs human review (conf %.2f)",
		in.NodeName, d.Confidence)

	rca := d.RootCause
	if rca == "" {
		rca = "(no root-cause text)"
	}

	gateReason := in.GateReason
	if gateReason == "" {
		gateReason = "(no gate reason recorded)"
	}

	kubectlBlock := fmt.Sprintf(
		"```\nkubectl --context=%s -n %s get nhd %s -o yaml\n```",
		in.ClusterName, fallback(in.Namespace, "cf-monitoring"), in.NHDName,
	)

	auditURL := ""
	if d.AuditLogRef != nil {
		auditURL = d.AuditLogRef.ObjectStore
	}

	msg := slackEnvelope{
		Text: header,
		Blocks: []slackBlock{
			{Type: "header", Text: &slackText{Type: "plain_text", Text: header}},
			{
				Type: "section",
				Fields: []slackText{
					{Type: "mrkdwn", Text: "*Cluster:* " + in.ClusterName},
					{Type: "mrkdwn", Text: "*Action:* NOT cordoned — needs review"},
				},
			},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Why the gate didn't fire:* " + gateReason}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*RCA:* " + rca}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Inspect:*\n" + kubectlBlock}},
		},
	}

	if auditURL != "" {
		msg.Blocks = append(msg.Blocks, slackBlock{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: "*Audit log:* " + auditURL},
		})
	}

	return json.Marshal(msg)
}

// BuildCritical returns the Slack JSON payload for a case that ended
// in terminal `Failed` after the FR-7 retry-once arm exhausted.
// Distinct from `Applied` and `HumanInLoop` so on-call can tell at a
// glance that the controller is "stuck" rather than silent: the
// header literally says AGENT FAILED, the action field reports the
// node was NOT cordoned, and the failure reason + detail surface
// verbatim. Per spec FR-9 this message uses a `severity=critical`
// framing — Slack incoming webhooks have no severity field, so the
// distinct text + Action: NOT cordoned line carry that semantic.
func BuildCritical(in CriticalInput) ([]byte, error) {
	if in.NodeName == "" {
		return nil, fmt.Errorf("BuildCritical: NodeName is required")
	}
	if in.FailureReason == "" {
		return nil, fmt.Errorf("BuildCritical: FailureReason is required")
	}

	header := fmt.Sprintf("NodeMedic: %s – AGENT FAILED", in.NodeName)
	attempts := in.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	attemptsLine := fmt.Sprintf("after %d attempt(s) — controller-level retry-once already burned", attempts)

	detail := in.FailureDetail
	if detail == "" {
		detail = "(no detail recorded)"
	}

	kubectlBlock := fmt.Sprintf(
		"```\nkubectl --context=%s -n %s get nhd %s -o yaml\n```",
		in.ClusterName, fallback(in.Namespace, "cf-monitoring"), in.NHDName,
	)

	msg := slackEnvelope{
		Text: header,
		Blocks: []slackBlock{
			{Type: "header", Text: &slackText{Type: "plain_text", Text: header}},
			{
				Type: "section",
				Fields: []slackText{
					{Type: "mrkdwn", Text: "*Cluster:* " + in.ClusterName},
					{Type: "mrkdwn", Text: "*Action:* NOT cordoned — agent never produced a usable diagnosis"},
				},
			},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Failure:* `%s` %s", in.FailureReason, attemptsLine)}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Detail:* " + detail}},
			{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Inspect:*\n" + kubectlBlock}},
		},
	}

	return json.Marshal(msg)
}

// slackEnvelope / slackBlock / slackText are the minimal Block Kit
// shapes we emit. Hand-built rather than depending on slack-go to
// keep the dep surface tight (research R-11).
type slackEnvelope struct {
	Text   string       `json:"text,omitempty"`
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type   string      `json:"type"`
	Text   *slackText  `json:"text,omitempty"`
	Fields []slackText `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
