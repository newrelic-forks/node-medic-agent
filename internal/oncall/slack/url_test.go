/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package slack

import (
	"strings"
	"testing"
)

// T026 — table-driven coverage for BuildCaseURL per
// .specify/specs/003-nodemedic-oncall-ui/contracts/slack-block-kit.md §URL-shape.
//
// Behavior locked by this test:
//   - happy path: base + nhdName joined with a single "/cases/" separator
//   - trailing slash on base URL is normalized away
//   - empty base or empty NHD name returns an error
//   - the helper does NOT URL-encode nhdName (Spec 001 names are path-safe by construction)
func TestBuildCaseURL(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		nhdName   string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "happy path",
			baseURL: "http://localhost:8080",
			nhdName: "cf1z-general-nodes-1000007-1718374200",
			want:    "http://localhost:8080/cases/cf1z-general-nodes-1000007-1718374200",
		},
		{
			name:    "trailing slash on base is normalized",
			baseURL: "http://localhost:8080/",
			nhdName: "foo-1234",
			want:    "http://localhost:8080/cases/foo-1234",
		},
		{
			name:    "https + custom port",
			baseURL: "https://nodemedic.cf.example.internal:9443",
			nhdName: "node-X-9999999999",
			want:    "https://nodemedic.cf.example.internal:9443/cases/node-X-9999999999",
		},
		{
			name:      "empty base URL fails",
			baseURL:   "",
			nhdName:   "foo-1234",
			wantErr:   true,
			errSubstr: "uiBaseURL",
		},
		{
			name:      "empty NHD name fails",
			baseURL:   "http://localhost:8080",
			nhdName:   "",
			wantErr:   true,
			errSubstr: "nhdName",
		},
		{
			name:      "both empty fails",
			baseURL:   "",
			nhdName:   "",
			wantErr:   true,
			errSubstr: "uiBaseURL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildCaseURL(tc.baseURL, tc.nhdName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BuildCaseURL(%q, %q) = %q, want error containing %q",
						tc.baseURL, tc.nhdName, got, tc.errSubstr)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("BuildCaseURL(%q, %q) error = %v, want substring %q",
						tc.baseURL, tc.nhdName, err, tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCaseURL(%q, %q) returned unexpected error: %v",
					tc.baseURL, tc.nhdName, err)
			}
			if got != tc.want {
				t.Fatalf("BuildCaseURL(%q, %q) = %q, want %q",
					tc.baseURL, tc.nhdName, got, tc.want)
			}
		})
	}
}
