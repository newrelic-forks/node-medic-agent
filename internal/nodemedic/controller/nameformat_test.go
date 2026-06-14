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
	"testing"
	"time"
)

func TestNameForCase(t *testing.T) {
	t.Parallel()

	// Fixed reference time: 2026-06-12T15:00:00Z = 1781276400 unix.
	ts := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	const tsSecs = "1781276400"

	// Per data-model §1.5: lowercase + replace any char outside
	// [a-z0-9-] with `-` + trim trailing `-`. Dots in EKS node names
	// become dashes.
	cases := []struct {
		name     string
		nodeName string
		want     string
	}{
		{
			name:     "EKS node name",
			nodeName: "ip-10-1-2-3.us-east-2.compute.internal",
			want:     "ip-10-1-2-3-us-east-2-compute-internal",
		},
		{
			name:     "Azure VM name",
			nodeName: "cf1z-worker-vm-04",
			want:     "cf1z-worker-vm-04",
		},
		{
			name:     "uppercase normalised + dots dashed",
			nodeName: "IP-10-0-0-1.EC2.INTERNAL",
			want:     "ip-10-0-0-1-ec2-internal",
		},
		{
			name:     "long name truncated to 50 chars",
			nodeName: "ip-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.compute.internal", // > 50
			want:     "",                                                                     // computed below
		},
		{
			name:     "trailing special chars stripped",
			nodeName: "node-with-trailing!!!",
			want:     "node-with-trailing",
		},
		{
			name:     "all special chars falls back",
			nodeName: "!@#$%^",
			want:     "node",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NameForCase(tc.nodeName, ts)

			// Truncation case computes its expected value here so the
			// test stays in sync with nameMaxNodeSegment.
			if tc.want == "" {
				if !strings.HasSuffix(got, "-"+tsSecs) {
					t.Fatalf("expected ts suffix %q in %q", "-"+tsSecs, got)
				}
				prefix := strings.TrimSuffix(got, "-"+tsSecs)
				if len(prefix) > nameMaxNodeSegment {
					t.Errorf("prefix len %d > %d: %q", len(prefix), nameMaxNodeSegment, prefix)
				}
				if !strings.HasPrefix(prefix, "ip-aaa") {
					t.Errorf("expected truncated name to retain leading chars, got %q", prefix)
				}
				return
			}

			want := tc.want + "-" + tsSecs
			if got != want {
				t.Errorf("NameForCase(%q, ts) = %q, want %q", tc.nodeName, got, want)
			}
		})
	}
}

func TestNameForCase_Deterministic(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	a := NameForCase("ip-10-1-2-3.us-east-2.compute.internal", ts)
	b := NameForCase("ip-10-1-2-3.us-east-2.compute.internal", ts)
	if a != b {
		t.Errorf("NameForCase non-deterministic: %q vs %q", a, b)
	}
}
