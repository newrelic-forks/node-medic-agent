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
	"time"
)

// nameMaxNodeSegment is the maximum length of the truncated node-name
// segment in the NHD name (data-model.md §1.5).
const nameMaxNodeSegment = 50

// NameForCase computes the deterministic NHD object name from a node
// name and the trigger's observedAt timestamp:
//
//	<sanitized-node-truncated-50>-<unix-ts-seconds>
//
// Determinism is the point: a duplicate flap that survives the
// debounce window will collide on Create and short-circuit at the
// apiserver (FR-3 idempotency). Every character outside [a-z0-9-] in
// the node name is replaced with `-`; trailing `-` is trimmed before
// the timestamp is appended.
func NameForCase(nodeName string, observedAt time.Time) string {
	return sanitizeNodeSegment(nodeName) + "-" + unixSeconds(observedAt)
}

func sanitizeNodeSegment(nodeName string) string {
	lowered := strings.ToLower(nodeName)
	if len(lowered) > nameMaxNodeSegment {
		lowered = lowered[:nameMaxNodeSegment]
	}
	var b strings.Builder
	b.Grow(len(lowered))
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		// Pathological case: node name was all special chars. Fall back
		// to a fixed token so we still produce a valid DNS-1123 label.
		return "node"
	}
	return out
}

func unixSeconds(t time.Time) string {
	// Format manually to avoid pulling in strconv just for this.
	secs := t.UTC().Unix()
	if secs < 0 {
		secs = 0
	}
	// Standard library route: strconv.FormatInt is fastest and clearest.
	return formatInt(secs)
}

// formatInt is strconv.FormatInt(n, 10) inlined to avoid the import in
// this single-use file. (It's idiomatic Go to use strconv here; this
// avoidance is a vanity preference, not a perf claim.)
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
