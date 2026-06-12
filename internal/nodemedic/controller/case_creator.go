/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/nodemedic/providerid"
)

// Trigger is the event captured from a NodeCondition transition that
// kicks off case creation.
type Trigger struct {
	Node        *corev1.Node
	ConditionType string
	Reason      string
	Message     string
	ObservedAt  time.Time
}

// MetadataResolutionError signals that one of provider/region/instanceId
// could not be derived from the Node. The reconciler emits a
// MetadataResolutionFailed Event when this fires (FR-2 last paragraph).
type MetadataResolutionError struct {
	Field  string // "provider" | "region" | "instanceId"
	Detail string
}

func (e *MetadataResolutionError) Error() string {
	return fmt.Sprintf("metadata resolution failed: %s: %s", e.Field, e.Detail)
}

// CreateCase resolves Node metadata, builds the NHD object, and
// Creates it. Idempotent on AlreadyExists (FR-3).
func CreateCase(
	ctx context.Context,
	c client.Client,
	t Trigger,
	clusterName, namespace string,
	maxTurns int32,
	maxBudgetUSD string,
	deadline time.Time,
) (*nodemedicv1alpha1.NodeHealthDiagnosisAI, error) {
	if t.Node == nil {
		return nil, errors.New("nil Node in Trigger")
	}

	provider, region, instanceID, err := resolveCloudMetadata(t.Node)
	if err != nil {
		return nil, err
	}

	nhd := &nodemedicv1alpha1.NodeHealthDiagnosisAI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameForCase(t.Node.Name, t.ObservedAt),
			Namespace: namespace,
		},
		Spec: nodemedicv1alpha1.NodeHealthDiagnosisAISpec{
			Case: nodemedicv1alpha1.CaseSpec{
				CaseId:      uuid.NewString(),
				NodeName:    t.Node.Name,
				ClusterName: clusterName,
				Provider:    provider,
				Region:      region,
				InstanceId:  instanceID,
				Trigger: nodemedicv1alpha1.TriggerSpec{
					Type:       t.ConditionType,
					Reason:     t.Reason,
					Message:    truncate(t.Message, 256),
					ObservedAt: metav1.NewTime(t.ObservedAt),
				},
			},
			Budgets: nodemedicv1alpha1.BudgetsSpec{
				MaxTurns:     maxTurns,
				MaxBudgetUSD: maxBudgetUSD,
				Deadline:     ptrTime(deadline),
			},
		},
	}

	if err := c.Create(ctx, nhd); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Idempotent path. Fetch the existing one so the caller can
			// keep reconciling against the canonical object.
			existing := &nodemedicv1alpha1.NodeHealthDiagnosisAI{}
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(nhd), existing); getErr != nil {
				return nil, fmt.Errorf("get after AlreadyExists: %w", getErr)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("create NHD: %w", err)
	}
	return nhd, nil
}

// resolveCloudMetadata implements FR-2's resolution table. Returns
// MetadataResolutionError if any required field is missing or
// unparseable.
func resolveCloudMetadata(node *corev1.Node) (
	provider nodemedicv1alpha1.Provider,
	region, instanceID string,
	err error,
) {
	provider, err = resolveProvider(node)
	if err != nil {
		return "", "", "", err
	}

	region = node.Labels["topology.kubernetes.io/region"]
	if region == "" {
		region = node.Labels["failure-domain.beta.kubernetes.io/region"]
	}
	if region == "" {
		return "", "", "", &MetadataResolutionError{
			Field:  "region",
			Detail: "neither topology.kubernetes.io/region nor failure-domain.beta.kubernetes.io/region is set",
		}
	}

	switch provider {
	case nodemedicv1alpha1.ProviderAWS:
		id, perr := providerid.ParseAWS(node.Spec.ProviderID)
		if perr != nil {
			return "", "", "", &MetadataResolutionError{
				Field:  "instanceId",
				Detail: perr.Error(),
			}
		}
		instanceID = id
	case nodemedicv1alpha1.ProviderAzure:
		// Azure parser lands in Phase 6 (US4 / T065). For US1 we error
		// out so an Azure-side trigger doesn't silently produce an
		// unparseable NHD.
		return "", "", "", &MetadataResolutionError{
			Field:  "instanceId",
			Detail: "azure providerID parser not yet implemented (US4)",
		}
	default:
		return "", "", "", &MetadataResolutionError{
			Field:  "provider",
			Detail: fmt.Sprintf("unsupported provider %q", provider),
		}
	}

	return provider, region, instanceID, nil
}

func resolveProvider(node *corev1.Node) (nodemedicv1alpha1.Provider, error) {
	// Preferred: argo-webhook-injected label.
	if v := node.Labels["cf.newrelic.com/cloud-provider"]; v != "" {
		switch v {
		case "aws":
			return nodemedicv1alpha1.ProviderAWS, nil
		case "azure":
			return nodemedicv1alpha1.ProviderAzure, nil
		default:
			return "", &MetadataResolutionError{
				Field:  "provider",
				Detail: fmt.Sprintf("cf.newrelic.com/cloud-provider=%q is not in {aws, azure}", v),
			}
		}
	}
	// Fallback: parse providerID URI scheme.
	pid := node.Spec.ProviderID
	switch {
	case strings.HasPrefix(pid, "aws://"):
		return nodemedicv1alpha1.ProviderAWS, nil
	case strings.HasPrefix(pid, "azure://"):
		return nodemedicv1alpha1.ProviderAzure, nil
	case pid == "":
		return "", &MetadataResolutionError{
			Field:  "provider",
			Detail: "empty spec.providerID and no cf.newrelic.com/cloud-provider label",
		}
	default:
		return "", &MetadataResolutionError{
			Field:  "provider",
			Detail: fmt.Sprintf("unrecognized providerID scheme: %q", pid),
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ptrTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}
