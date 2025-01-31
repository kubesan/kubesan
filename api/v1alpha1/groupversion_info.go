// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the v1alpha1 API group
// +kubebuilder:object:generate=true
// +kubebuilder:validation:Required
// +groupName=kubesan.gitlab.io
package v1alpha1

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	Group = getGroup()

	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func getGroup() string {
	group := os.Getenv("KUBESAN_GROUP")
	if group == "" {
		group = "kubesan.gitlab.io"
	}
	return group
}
