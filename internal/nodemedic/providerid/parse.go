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
// In Phase 3 (US1) this package only handles AWS. The Azure parser
// lands in Phase 6 (US4 / T065) when we re-target the dispatcher to
// route on the URI scheme.
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
