/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1alpha1 contains API Schema definitions for the
// nodemedic.cf.newrelic.com v1alpha1 API group. The schema is owned by
// Scope 3 per Constitution Article II.2; this Go package mirrors the
// frozen schema vendored at
// .specify/specs/001-nodemedic-controller/contracts/nhd-crd.yaml.
//
// +kubebuilder:object:generate=true
// +groupName=nodemedic.cf.newrelic.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group/version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "nodemedic.cf.newrelic.com", Version: "v1alpha1"}

// SchemeBuilder collects the registration funcs used by AddToScheme.
//
// Using the apimachinery runtime.SchemeBuilder rather than
// sigs.k8s.io/controller-runtime/pkg/scheme.Builder so this package can
// compile without dragging in controller-runtime; the manager (T014)
// imports controller-runtime separately and calls AddToScheme on its
// own runtime.Scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme registers this group/version's types into a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&NodeHealthDiagnosisAI{},
		&NodeHealthDiagnosisAIList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
