// T055 — sanity tests for the FR-17 defensive belt and the
// FR-17a NotFound predicate. The helpers are unexported, so we
// reach them via the integration layer's behavior; this file
// covers what's reachable from outside the package.
//
// Specifically: confirm the wire-level audit.UIActionEntry shape +
// the contract enum constraints. Heavier behavior tests live in the
// integration suite (T056, T057).
package unit

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/node-problem-detector/internal/oncall/audit"
)

func TestUIActionEntry_JSONShape(t *testing.T) {
	e := audit.UIActionEntry{
		Ts:     time.Date(2026, 6, 14, 15, 42, 11, 0, time.UTC),
		Action: "uncordon",
		Actor:  "demo-anonymous",
		Result: "ok",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"ts":"2026-06-14T15:42:11Z","action":"uncordon","actor":"demo-anonymous","result":"ok"}`
	if string(b) != want {
		t.Errorf("\n got = %s\nwant = %s", string(b), want)
	}
}

func TestUIActionEntry_DetailOmittedWhenEmpty(t *testing.T) {
	e := audit.UIActionEntry{
		Ts:     time.Date(2026, 6, 14, 15, 42, 11, 0, time.UTC),
		Action: "uncordon",
		Actor:  "demo-anonymous",
		Result: "ok",
	}
	b, _ := json.Marshal(e)
	if got := string(b); contains(got, `"detail"`) {
		t.Errorf("empty detail must omit the key; got %s", got)
	}
}

func TestUIActionEntry_DetailIncludedWhenSet(t *testing.T) {
	e := audit.UIActionEntry{
		Ts:     time.Date(2026, 6, 14, 15, 42, 11, 0, time.UTC),
		Action: "drain",
		Actor:  "demo-anonymous",
		Result: "partial",
		Detail: "evicted=12 skipped=3 errored=1",
	}
	b, _ := json.Marshal(e)
	if got := string(b); !contains(got, `"detail":"evicted=12 skipped=3 errored=1"`) {
		t.Errorf("detail key missing; got %s", got)
	}
}

func TestUIActionEntry_ValidateAllResultEnums(t *testing.T) {
	for _, r := range []string{"ok", "reclaimed", "error", "partial"} {
		e := audit.UIActionEntry{
			Ts: time.Now().UTC(), Action: "uncordon", Actor: "demo-anonymous", Result: r,
		}
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(result=%q) = %v, want nil", r, err)
		}
	}
}

func TestUIActionEntry_ValidateAllActionEnums(t *testing.T) {
	for _, a := range []string{"uncordon", "drain", "clear-skip-deletion"} {
		e := audit.UIActionEntry{
			Ts: time.Now().UTC(), Action: a, Actor: "demo-anonymous", Result: "ok",
		}
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(action=%q) = %v, want nil", a, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
