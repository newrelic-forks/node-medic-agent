// Package unit holds unit tests for the on-call UI binary.
package unit

import (
	"strings"
	"testing"

	"k8s.io/node-problem-detector/internal/oncall/server"
)

// TestValidateClusterName covers the FR-23 / Constitution Article I.5
// guard. The allowlist is `cf1z`, `jc1z`, `sk1z`, or any string with a
// `test-` prefix; everything else MUST be rejected.
//
// The empty-suffix case `test-` is intentionally accepted as a degenerate
// boundary: the chart's _helpers.tpl macro accepts the same shape, and we
// don't want the binary to disagree with the chart on edge cases.
func TestValidateClusterName(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		errSubstr string
	}{
		// Accepted — all four legacy CF Azure kubeadm names and the
		// test-* prefix.
		{name: "cf1z accepted", input: "cf1z", wantErr: false},
		{name: "jc1z accepted", input: "jc1z", wantErr: false},
		{name: "sk1z accepted", input: "sk1z", wantErr: false},
		{name: "test-foo accepted", input: "test-foo", wantErr: false},
		{name: "test- empty suffix accepted (boundary, matches chart helper)", input: "test-", wantErr: false},

		// Rejected — empty + production-shape names.
		{name: "empty rejected", input: "", wantErr: true, errSubstr: "required"},
		{name: "stg-going-plaid rejected", input: "stg-going-plaid", wantErr: true, errSubstr: "Article I.5"},
		{name: "us-big-cone rejected", input: "us-big-cone", wantErr: true, errSubstr: "Article I.5"},
		{name: "eu-lesser-forest rejected", input: "eu-lesser-forest", wantErr: true, errSubstr: "Article I.5"},
		{name: "production-cluster rejected", input: "production-cluster", wantErr: true, errSubstr: "Article I.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := server.ValidateClusterName(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateClusterName(%q) returned nil, want error", tc.input)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("ValidateClusterName(%q) error = %q; want substring %q", tc.input, err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateClusterName(%q) returned %v, want nil", tc.input, err)
			}
		})
	}
}
