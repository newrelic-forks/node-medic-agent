/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package notifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Slack posts a pre-built Block Kit JSON payload to a Slack incoming
// webhook. Per spec FR-9: 3 attempts at 1s/2s/4s backoff,
// 5s per-attempt timeout. Final failure surfaces as an error so the
// reconciler can emit a NotifierFailed Event (FR-9 last paragraph).
type Slack struct {
	WebhookURL string
	HTTP       *http.Client
	Backoffs   []time.Duration
	Sleep      func(time.Duration) // injectable for tests
}

// NewSlack returns a Slack notifier with spec FR-9 defaults.
func NewSlack(webhookURL string) *Slack {
	return &Slack{
		WebhookURL: webhookURL,
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Backoffs:   []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		Sleep:      time.Sleep,
	}
}

// PostResult bundles attempt count + last error for the metrics path.
type PostResult struct {
	Attempts int
	Posted   bool
	LastErr  error
}

// Post sends the payload, retrying transient failures (5xx and
// network errors) per Backoffs. Returns nil err on success; the
// PostResult always reflects the actual attempts made.
func (s *Slack) Post(ctx context.Context, payload []byte) PostResult {
	if s.WebhookURL == "" {
		return PostResult{LastErr: errors.New("slack webhook URL is empty")}
	}

	res := PostResult{}
	for i := 0; i <= len(s.Backoffs); i++ {
		if i > 0 {
			delay := s.Backoffs[i-1]
			if s.Sleep != nil {
				s.Sleep(delay)
			} else {
				time.Sleep(delay)
			}
		}
		res.Attempts = i + 1

		err := s.doOnce(ctx, payload)
		if err == nil {
			res.Posted = true
			res.LastErr = nil
			return res
		}
		res.LastErr = err
		// Always retry transient (5xx / network). Slack webhook errors
		// for malformed payloads (4xx) are not retry-eligible — but
		// 4xx means we wrote a bad message, so failing fast surfaces
		// the bug instead of hammering the webhook.
		if !isRetryable(err) {
			return res
		}
	}
	return res
}

func (s *Slack) doOnce(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return &transientErr{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain so connections can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 500 {
		return &transientErr{err: fmt.Errorf("slack %d: %s", resp.StatusCode, body)}
	}
	return fmt.Errorf("slack rejected payload: %d %s", resp.StatusCode, body)
}

type transientErr struct{ err error }

func (e *transientErr) Error() string { return e.err.Error() }
func (e *transientErr) Unwrap() error { return e.err }
func (e *transientErr) Transient()    {}

func isRetryable(err error) bool {
	var t interface{ Transient() }
	return errors.As(err, &t)
}
