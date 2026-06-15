// Package server assembles the on-call UI's HTTP surface: the http.Server,
// route table, html/template loader, structured logger, and middleware
// (request-id, access logging, recovery).
package server

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateClusterName enforces Constitution Article I.5 (non-production
// clusters only) at the binary level. The on-call UI refuses to start
// unless --cluster-name (or env CLUSTER_NAME) is one of:
//
//   - "cf1z" (Container Fabric legacy Azure kubeadm test cluster)
//   - "jc1z" (Container Fabric legacy Azure kubeadm test cluster)
//   - "sk1z" (Container Fabric legacy Azure kubeadm test cluster)
//   - any name with a "test-" prefix (AWS/EKS or Azure test clusters,
//     e.g. "test-odd-wire" or "test-foo")
//
// Anything else — empty, "stg-*", "us-*", "eu-*", or other production
// shapes — is rejected before the HTTP server comes up. This is the
// binary half of the belt-and-suspenders cluster-name guard; the chart
// helper template (deployment/helm/nodemedic-oncall-ui/templates/_helpers.tpl)
// is the chart half.
//
// The allowlist matches the constitution + the agent chart helper +
// data-model §9. The Spec 001 controller binary's `validateClusterName`
// today only accepts cf1z + test-*; that's a Spec 001 follow-up and
// MUST NOT propagate here.
func ValidateClusterName(name string) error {
	if name == "" {
		return errors.New("--cluster-name is required (Constitution Article I.5: non-production clusters only)")
	}
	switch name {
	case "cf1z", "jc1z", "sk1z":
		return nil
	}
	if strings.HasPrefix(name, "test-") {
		return nil
	}
	return fmt.Errorf("--cluster-name=%q must be one of cf1z/jc1z/sk1z (legacy CF Azure kubeadm test clusters) "+
		"or start with test- (AWS/EKS or Azure test clusters) "+
		"(Constitution Article I.5: non-production clusters only)", name)
}
