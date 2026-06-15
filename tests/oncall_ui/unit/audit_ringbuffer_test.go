// T054 — table-driven tests for the FIFO ring buffer per data-model.md
// §3 + research R-6. AppendEntry MUST keep the slice ≤ N entries,
// dropping from the front when over.
package unit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/node-problem-detector/internal/oncall/audit"
)

func TestAppendEntry_EmptyAnnotation_AddsOne(t *testing.T) {
	got, err := audit.AppendEntry("", entry("2026-06-14T15:42:00Z", "uncordon", "ok"), 20)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries) != 1 {
		t.Errorf("len = %d, want 1", len(entries))
	}
	if entries[0].Action != "uncordon" {
		t.Errorf("entry[0].Action = %q, want uncordon", entries[0].Action)
	}
}

func TestAppendEntry_NineteenPlusOne_ReachesTwenty(t *testing.T) {
	in := makeEntries(19)
	raw := encode(t, in)
	got, err := audit.AppendEntry(raw, entry("2026-06-14T16:00:00Z", "uncordon", "ok"), 20)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries) != 20 {
		t.Errorf("len = %d, want 20 (19 + 1 stays under cap)", len(entries))
	}
}

func TestAppendEntry_TwentyPlusOne_DropsOldest(t *testing.T) {
	in := makeEntries(20)
	raw := encode(t, in)
	newEntry := entry("2026-06-14T16:01:00Z", "uncordon", "ok")
	got, err := audit.AppendEntry(raw, newEntry, 20)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries) != 20 {
		t.Errorf("len = %d, want 20 (FIFO cap held)", len(entries))
	}
	// Oldest (entries[0] in the input) must be gone; newest must be at the end.
	if entries[0].Ts.Equal(in[0].Ts) {
		t.Errorf("oldest entry not dropped: entries[0].Ts = %v == in[0].Ts", entries[0].Ts)
	}
	if !entries[19].Ts.Equal(newEntry.Ts) {
		t.Errorf("newest entry not at tail: entries[19].Ts = %v, want %v", entries[19].Ts, newEntry.Ts)
	}
}

func TestAppendEntry_TwentyFiveDegenerate_TruncatesToTwenty(t *testing.T) {
	in := makeEntries(25)
	raw := encode(t, in)
	got, err := audit.AppendEntry(raw, entry("2026-06-14T16:30:00Z", "uncordon", "ok"), 20)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries) != 20 {
		t.Errorf("len = %d, want 20 (25+1 → cap to 20)", len(entries))
	}
}

func TestAppendEntry_TotalSizeUnder8KB(t *testing.T) {
	// Fill 20 entries with maxLength detail; verify total bytes < 8192
	// (AC-14b validation threshold).
	raw := ""
	for i := 0; i < 20; i++ {
		e := entry(time.Now().Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339), "drain", "partial")
		e.Detail = strings.Repeat("x", audit.MaxDetailLen)
		updated, err := audit.AppendEntry(raw, e, 20)
		if err != nil {
			t.Fatalf("AppendEntry %d: %v", i, err)
		}
		raw = updated
	}
	if len(raw) >= 8192 {
		t.Errorf("ring buffer size = %d bytes, want < 8192 (AC-14b)", len(raw))
	}
}

func TestAppendEntry_DefaultBufferSize(t *testing.T) {
	in := makeEntries(20)
	raw := encode(t, in)
	got, err := audit.AppendEntry(raw, entry("2026-06-14T16:01:00Z", "uncordon", "ok"), 0 /* default */)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries) != audit.DefaultBufferSize {
		t.Errorf("len = %d, want default %d", len(entries), audit.DefaultBufferSize)
	}
}

func TestAppendEntry_ValidationRejects_BadAction(t *testing.T) {
	bad := entry("2026-06-14T15:00:00Z", "delete-everything", "ok")
	_, err := audit.AppendEntry("", bad, 20)
	if err == nil {
		t.Error("AppendEntry(action=delete-everything) returned nil, want validation error")
	}
}

func TestAppendEntry_ValidationRejects_BadResult(t *testing.T) {
	bad := entry("2026-06-14T15:00:00Z", "uncordon", "weird")
	_, err := audit.AppendEntry("", bad, 20)
	if err == nil {
		t.Error("AppendEntry(result=weird) returned nil, want validation error")
	}
}

func TestAppendEntry_TruncatesLongDetail(t *testing.T) {
	e := entry("2026-06-14T15:00:00Z", "drain", "partial")
	e.Detail = strings.Repeat("x", audit.MaxDetailLen+50)
	got, err := audit.AppendEntry("", e, 20)
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	entries := decode(t, got)
	if len(entries[0].Detail) > audit.MaxDetailLen {
		t.Errorf("Detail not truncated: len = %d", len(entries[0].Detail))
	}
}

func TestReadEntries_EmptyAndMalformed(t *testing.T) {
	got, err := audit.ReadEntries("")
	if err != nil || len(got) != 0 {
		t.Errorf("ReadEntries(\"\") = (%v, %v), want ([], nil)", got, err)
	}
	got, err = audit.ReadEntries("not-json")
	if err == nil {
		t.Error("ReadEntries(\"not-json\") returned nil error, want decode error")
	}
	if got != nil {
		t.Errorf("ReadEntries(malformed) = %v, want nil slice on error", got)
	}
}

// ----- helpers -----

func entry(ts, action, result string) audit.UIActionEntry {
	t, _ := time.Parse(time.RFC3339, ts)
	return audit.UIActionEntry{
		Ts:     t,
		Action: action,
		Actor:  "demo-anonymous",
		Result: result,
	}
}

func makeEntries(n int) []audit.UIActionEntry {
	base, _ := time.Parse(time.RFC3339, "2026-06-14T15:00:00Z")
	out := make([]audit.UIActionEntry, n)
	for i := 0; i < n; i++ {
		out[i] = audit.UIActionEntry{
			Ts:     base.Add(time.Duration(i) * time.Second),
			Action: "uncordon",
			Actor:  "demo-anonymous",
			Result: "ok",
		}
	}
	return out
}

func encode(t *testing.T, entries []audit.UIActionEntry) string {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func decode(t *testing.T, raw string) []audit.UIActionEntry {
	t.Helper()
	var out []audit.UIActionEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return out
}
