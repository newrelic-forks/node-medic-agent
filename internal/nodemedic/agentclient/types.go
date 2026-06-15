/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package agentclient is the controller-side client for the
// `POST /diagnose` HTTP contract. The exact request/response shape
// lives at .specify/specs/001-nodemedic-controller/contracts/post-diagnose.md
// — golden-file tests in this package lock the on-the-wire JSON.
package agentclient

// DiagnoseRequest is the body of `POST /diagnose`. JSON tags MUST stay
// byte-stable; the contract file is the source of truth and the
// golden-file test enforces it.
type DiagnoseRequest struct {
	CaseId      string          `json:"caseId"`
	NodeName    string          `json:"nodeName"`
	ClusterName string          `json:"clusterName"`
	Provider    string          `json:"provider"`
	Region      string          `json:"region"`
	InstanceId  string          `json:"instanceId"`
	Trigger     DiagnoseTrigger `json:"trigger"`
	Budgets     DiagnoseBudgets `json:"budgets"`
}

// DiagnoseTrigger mirrors the NPD-flipped condition payload.
type DiagnoseTrigger struct {
	Type       string `json:"type"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	ObservedAt string `json:"observedAt"` // RFC3339
}

// DiagnoseBudgets bounds the agent loop.
type DiagnoseBudgets struct {
	MaxTurns     int    `json:"maxTurns"`
	MaxBudgetUSD string `json:"maxBudgetUSD"`
	DeadlineSec  int    `json:"deadlineSec"`
}

// DiagnoseResponse is the agent's 202 body. `status` is "queued" or
// "completed"; the controller does not branch on it (research R-5).
type DiagnoseResponse struct {
	CaseId string `json:"caseId"`
	Status string `json:"status"`
}
