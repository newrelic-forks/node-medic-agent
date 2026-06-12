/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package providerid

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestParseAWS(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		providerID string
		want       string
		wantErr    error
	}{
		{
			name:       "valid EKS providerID",
			providerID: "aws:///us-east-2a/i-0abc1234deadbeef",
			want:       "i-0abc1234deadbeef",
		},
		{
			name:       "valid with hyphenated AZ",
			providerID: "aws:///us-west-2c/i-0123456789abcdef0",
			want:       "i-0123456789abcdef0",
		},
		{
			name:       "empty providerID",
			providerID: "",
			wantErr:    ErrEmpty,
		},
		{
			name:       "azure scheme rejected",
			providerID: "azure:///subscriptions/abc/resourceGroups/x/providers/Microsoft.Compute/virtualMachines/vm-01",
			wantErr:    ErrUnsupportedScheme,
		},
		{
			name:       "gce scheme rejected",
			providerID: "gce://my-project/us-central1-a/instance-1",
			wantErr:    ErrUnsupportedScheme,
		},
		{
			name:       "missing instance segment",
			providerID: "aws:///us-east-2a/",
			wantErr:    ErrMalformed,
		},
		{
			name:       "missing AZ segment",
			providerID: "aws:///i-0abc1234deadbeef",
			wantErr:    ErrMalformed,
		},
		{
			name:       "instance id without i- prefix",
			providerID: "aws:///us-east-2a/0abc1234deadbeef",
			wantErr:    ErrMalformed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAWS(tc.providerID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseAWS(%q) error = %v, want sentinel %v", tc.providerID, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAWS(%q) unexpected error: %v", tc.providerID, err)
			}
			if got != tc.want {
				t.Errorf("ParseAWS(%q) = %q, want %q", tc.providerID, got, tc.want)
			}
		})
	}
}

// TestParseAWS_FromFixture exercises the committed EKS Node fixture so
// any drift between the fixture's providerID shape and what the parser
// expects is caught in CI rather than at demo time.
func TestParseAWS_FromFixture(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "test", "nodemedic", "fixtures", "node-eks.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}

	var node corev1.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	if node.Spec.ProviderID == "" {
		t.Fatal("fixture has empty spec.providerID — this would also break FR-2 at runtime")
	}

	id, err := ParseAWS(node.Spec.ProviderID)
	if err != nil {
		t.Fatalf("ParseAWS(fixture providerID = %q): %v", node.Spec.ProviderID, err)
	}
	if id == "" {
		t.Fatal("ParseAWS returned empty instance id")
	}
	t.Logf("fixture parsed OK: providerID=%q instanceID=%q", node.Spec.ProviderID, id)
}
