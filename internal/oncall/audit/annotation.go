// Package audit ships the on-call UI's audit-trail surface: the
// ui-action-history annotation FIFO ring buffer and the structured-
// stdout log line emitted per action (FR-18 + FR-18a).
package audit

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AnnotationKey is the metadata.annotations key the UI writes per
// successful action. Read paths and the controller's status.action
// surface are merged in render/data.go (Phase 7 / T086).
const AnnotationKey = "nodemedic.cf.newrelic.com/ui-action-history"

// DefaultBufferSize is the FIFO ring buffer cap (FR-18a, AC-14b).
// `appendEntry` evicts oldest entries when adding the (Cap+1)th.
const DefaultBufferSize = 20

// MaxDetailLen caps the detail field's byte length per
// contracts/ui-action-history.schema.json (maxLength=200).
const MaxDetailLen = 200

// UIActionEntry mirrors contracts/ui-action-history.schema.json's
// UIActionEntry definition. JSON tags are explicit so the on-the-
// wire encoding stays byte-stable (no struct-field reorder regressions).
type UIActionEntry struct {
	Ts     time.Time `json:"ts"`
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
}

// Validate returns a non-nil error if the entry doesn't satisfy the
// contract's enum constraints. Callers (handlers) should reject
// malformed inputs at construction time, before patching.
func (e UIActionEntry) Validate() error {
	switch e.Action {
	case "uncordon", "drain", "clear-skip-deletion":
	default:
		return fmt.Errorf("audit: action %q must be one of uncordon|drain|clear-skip-deletion", e.Action)
	}
	switch e.Result {
	case "ok", "reclaimed", "error", "partial":
	default:
		return fmt.Errorf("audit: result %q must be one of ok|reclaimed|error|partial", e.Result)
	}
	if strings.TrimSpace(e.Actor) == "" {
		return fmt.Errorf("audit: actor must be non-empty (v1: %q)", "demo-anonymous")
	}
	if e.Ts.IsZero() {
		return fmt.Errorf("audit: ts must be set")
	}
	if len(e.Detail) > MaxDetailLen {
		// Don't reject — silently truncate at the maxLength
		// threshold per contract; the validator returns nil so
		// callers can still patch.
		e.Detail = e.Detail[:MaxDetailLen]
		_ = e.Detail
	}
	return nil
}

// ReadEntries decodes the existing annotation value into a slice. An
// empty annotation returns an empty slice + nil error. A malformed
// annotation returns a nil slice + the decode error; callers
// typically tolerate this (the UI page still renders without the
// audit history).
func ReadEntries(raw string) ([]UIActionEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []UIActionEntry{}, nil
	}
	var entries []UIActionEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("audit: decode annotation: %w", err)
	}
	return entries, nil
}

// AppendEntry decodes the existing annotation, appends the new entry,
// caps the slice to bufSize per the FIFO ring buffer rule, and
// returns the new JSON-encoded annotation value.
//
// If bufSize <= 0, DefaultBufferSize is used.
//
// FIFO invariant: when len(after-append) > cap, drop the OLDEST
// entries from the front. A degenerate input slice already over
// cap (e.g., 25 entries from a buggy past write) is also truncated
// to the last cap entries.
func AppendEntry(rawAnnotation string, entry UIActionEntry, bufSize int) (string, error) {
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	if err := entry.Validate(); err != nil {
		return "", err
	}
	if len(entry.Detail) > MaxDetailLen {
		entry.Detail = entry.Detail[:MaxDetailLen]
	}

	entries, err := ReadEntries(rawAnnotation)
	if err != nil {
		// Defensive — malformed existing annotation. Replace with
		// a fresh single-entry buffer so the audit trail isn't
		// indefinitely poisoned.
		entries = nil
	}
	entries = append(entries, entry)
	if len(entries) > bufSize {
		entries = entries[len(entries)-bufSize:]
	}

	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("audit: encode annotation: %w", err)
	}
	return string(b), nil
}
