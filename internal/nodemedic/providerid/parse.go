/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package providerid parses the `spec.providerID` URI on a Kubernetes
// Node and returns the cloud-specific instance identifier per spec
// FR-2.
//
// AWS shape: `aws:///<az>/<instance-id>` — last segment is the
// EC2 instance id.
// Azure shapes (cloud-provider-azure):
//   - Standalone VM:
//     `azure:///subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<vm-name>`
//   - VM Scale Set instance:
//     `azure:///subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachineScaleSets/<vmss>/virtualMachines/<instance-id>`
//
// ParseAzure returns the last segment after `/virtualMachines/` in
// either shape.
package providerid

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmpty signals an empty or missing providerID. Wrapped by callers
// that want to emit a structured Node Event.
var ErrEmpty = errors.New("empty providerID")

// ErrUnsupportedScheme signals a URI scheme outside the AWS/Azure
// allowlist (e.g. `gce://`).
var ErrUnsupportedScheme = errors.New("unsupported providerID scheme")

// ErrMalformed signals a recognised scheme but bad path shape (e.g.
// `aws:///us-east-2a/` with no instance id).
var ErrMalformed = errors.New("malformed providerID")

// ParseAWS extracts the AWS EC2 instance id from a providerID of the
// form `aws:///<az>/<instance-id>`. The instance id is the segment
// after the last `/`.
//
// Returns ErrEmpty / ErrUnsupportedScheme / ErrMalformed (wrapped with
// %w) so callers can branch on errors.Is.
func ParseAWS(providerID string) (instanceID string, err error) {
	if providerID == "" {
		return "", ErrEmpty
	}
	if !strings.HasPrefix(providerID, "aws://") {
		return "", fmt.Errorf("%w: %q (expected `aws://` prefix)", ErrUnsupportedScheme, providerID)
	}
	// EKS managed nodes: `aws:///<az>/<instance-id>` (note triple slash
	// — the host is empty). The trailing path segment is the instance.
	rest := strings.TrimPrefix(providerID, "aws://")
	rest = strings.TrimPrefix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("%w: %q (expected `aws:///<az>/<instance-id>`)", ErrMalformed, providerID)
	}
	instanceID = parts[len(parts)-1]
	if instanceID == "" {
		return "", fmt.Errorf("%w: %q (empty instance id)", ErrMalformed, providerID)
	}
	if !strings.HasPrefix(instanceID, "i-") {
		return "", fmt.Errorf("%w: %q (instance id must start with `i-`, got %q)", ErrMalformed, providerID, instanceID)
	}
	return instanceID, nil
}

// azureVMSegment is the marker we split on. Both standalone-VM and
// VMSS-instance providerIDs end with `/virtualMachines/<name>`.
const azureVMSegment = "/virtualMachines/"

// ParseAzure extracts the Azure VM instance identifier from a
// providerID that begins with `azure://` and contains
// `/virtualMachines/<name>` as its trailing segment.
//
// For standalone VMs the returned name is the VM resource name
// (matches `Node.Name` on cloud-provider-azure-managed clusters).
// For VMSS instances it's the per-instance numeric/alphabetic id
// (combined with the VMSS name to be globally unique). Either way,
// it's what the agent needs to dispatch the Azure-side Cloud-Info
// MCP backend.
func ParseAzure(providerID string) (instanceID string, err error) {
	if providerID == "" {
		return "", ErrEmpty
	}
	if !strings.HasPrefix(providerID, "azure://") {
		return "", fmt.Errorf("%w: %q (expected `azure://` prefix)", ErrUnsupportedScheme, providerID)
	}

	// Use LastIndex so VMSS shapes (which contain
	// `/virtualMachineScaleSets/.../virtualMachines/<n>`) resolve to
	// the trailing instance id, not the VMSS name.
	idx := strings.LastIndex(providerID, azureVMSegment)
	if idx < 0 {
		return "", fmt.Errorf("%w: %q (missing `%s` segment)", ErrMalformed, providerID, azureVMSegment)
	}
	tail := providerID[idx+len(azureVMSegment):]

	// Defensive: trim any trailing `/` (shouldn't happen on real
	// cloud-provider-azure output, but we don't trust the wire).
	tail = strings.TrimRight(tail, "/")

	if tail == "" {
		return "", fmt.Errorf("%w: %q (empty VM name after `%s`)", ErrMalformed, providerID, azureVMSegment)
	}
	if strings.Contains(tail, "/") {
		return "", fmt.Errorf("%w: %q (unexpected `/` after VM segment in %q)", ErrMalformed, providerID, tail)
	}
	return tail, nil
}
