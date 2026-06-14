/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResultClass categorises the outcome of a Diagnose attempt for
// metrics + reconciler dispatch (FR-7 retry path).
type ResultClass int

const (
	// ResultOK — agent returned 202.
	ResultOK ResultClass = iota
	// ResultRetry — 429 (concurrency cap). Caller may retry.
	ResultRetry
	// ResultServer — 5xx (agent crash). Caller may retry.
	ResultServer
	// ResultTimeout — network timeout / connection refused.
	ResultTimeout
	// ResultBadRequest — 400 (malformed). Do NOT retry.
	ResultBadRequest
	// ResultUnauthorized — 401 (bad token). Do NOT retry.
	ResultUnauthorized
	// ResultOther — unexpected status code.
	ResultOther
)

// String returns the lowercase metric label.
func (r ResultClass) String() string {
	switch r {
	case ResultOK:
		return "ok"
	case ResultRetry:
		return "429"
	case ResultServer:
		return "5xx"
	case ResultTimeout:
		return "timeout"
	case ResultBadRequest:
		return "bad_request"
	case ResultUnauthorized:
		return "unauthorized"
	default:
		return "other"
	}
}

// AttemptError is returned by Diagnose when the call fails. It carries
// the structured ResultClass so the reconciler can branch without
// pattern-matching error strings.
type AttemptError struct {
	Class      ResultClass
	StatusCode int
	Body       string // truncated to 4 KB
	Err        error  // underlying transport error, if any
}

func (e *AttemptError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("agent POST /diagnose: %s (%v)", e.Class, e.Err)
	}
	return fmt.Sprintf("agent POST /diagnose: %s (status=%d body=%q)", e.Class, e.StatusCode, e.Body)
}

func (e *AttemptError) Unwrap() error { return e.Err }

// Client posts diagnoses to the agent. Pure HTTP — no kube types,
// no global state. Construct once at startup and reuse.
type Client struct {
	URL         string        // e.g. http://nodemedic-agent.cf-monitoring.svc:8080/diagnose
	BearerToken string        // mounted via Secret/nodemedic-agent-token
	HTTP        *http.Client  // injectable for tests; defaults to a 1.5s-timeout client
	Backoffs    []time.Duration // 1s, 2s, 4s by default; use nil to disable retries
	UserAgent   string        // sent verbatim if non-empty
	Sleep       func(time.Duration) // injectable sleep for tests; nil → time.Sleep
}

// New returns a Client with sensible defaults: 1.5s per-attempt
// timeout, exp 1s/2s/4s backoffs, real wall-clock sleep.
func New(url, token string) *Client {
	return &Client{
		URL:         url,
		BearerToken: token,
		HTTP:        &http.Client{Timeout: 1500 * time.Millisecond},
		Backoffs:    []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		UserAgent:   "nodemedic-controller/0.1",
		Sleep:       time.Sleep,
	}
}

// Diagnose posts the request and returns the agent's response. On
// retry-eligible failures (429, 5xx, timeout) it backs off per
// c.Backoffs and retries. On 400/401 it returns immediately. The
// returned AttemptError describes the FINAL attempt's class.
func (c *Client) Diagnose(ctx context.Context, req *DiagnoseRequest) (*DiagnoseResponse, *AttemptError) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &AttemptError{Class: ResultBadRequest, Err: fmt.Errorf("marshal: %w", err)}
	}

	// First attempt + len(Backoffs) retries = up to len+1 attempts.
	var lastErr *AttemptError
	for i := 0; i <= len(c.Backoffs); i++ {
		if i > 0 {
			delay := c.Backoffs[i-1]
			if c.Sleep != nil {
				c.Sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}

		resp, attErr := c.doOnce(ctx, body)
		if attErr == nil {
			return resp, nil
		}
		lastErr = attErr

		// Terminal classes — do not retry.
		if attErr.Class == ResultBadRequest || attErr.Class == ResultUnauthorized || attErr.Class == ResultOther {
			return nil, attErr
		}
		// Retry-eligible: 429, 5xx, timeout. Loop continues.
	}
	return nil, lastErr
}

// doOnce performs a single attempt without retry.
func (c *Client) doOnce(ctx context.Context, body []byte) (*DiagnoseResponse, *AttemptError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &AttemptError{Class: ResultOther, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net.Error covers both timeout and connection refused. We
		// classify both as Timeout for retry purposes.
		var netErr interface{ Timeout() bool }
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, &AttemptError{Class: ResultTimeout, Err: err}
		}
		// Treat connection refused / DNS failures as Timeout-class so
		// the reconciler retries the same way.
		return nil, &AttemptError{Class: ResultTimeout, Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyStr := string(bodyBytes)

	switch {
	case resp.StatusCode == http.StatusAccepted:
		var out DiagnoseResponse
		if err := json.Unmarshal(bodyBytes, &out); err != nil {
			// 202 with malformed body — treat as Other (no retry).
			return nil, &AttemptError{Class: ResultOther, StatusCode: resp.StatusCode, Body: bodyStr, Err: err}
		}
		return &out, nil
	case resp.StatusCode == http.StatusBadRequest:
		return nil, &AttemptError{Class: ResultBadRequest, StatusCode: resp.StatusCode, Body: bodyStr}
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &AttemptError{Class: ResultUnauthorized, StatusCode: resp.StatusCode, Body: bodyStr}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &AttemptError{Class: ResultRetry, StatusCode: resp.StatusCode, Body: bodyStr}
	case resp.StatusCode == http.StatusConflict:
		// Idempotency: agent says "already in flight" — controller
		// treats as 202 (research R-5).
		return &DiagnoseResponse{CaseId: requestCaseID(body), Status: "queued"}, nil
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return nil, &AttemptError{Class: ResultServer, StatusCode: resp.StatusCode, Body: bodyStr}
	default:
		return nil, &AttemptError{Class: ResultOther, StatusCode: resp.StatusCode, Body: bodyStr}
	}
}

// requestCaseID extracts the `caseId` field from a JSON request body
// without re-unmarshalling the whole DiagnoseRequest. Used only on the
// 409 idempotency branch where we synthesize a response.
func requestCaseID(body []byte) string {
	var probe struct {
		CaseId string `json:"caseId"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.CaseId
}
