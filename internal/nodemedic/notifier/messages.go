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
// In Phase 3 (US1) only BuildApplied lands. BuildHumanInLoop arrives
// in Phase 4 (US2 / T053); BuildCritical in Phase 5 (US3 / T058).
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
