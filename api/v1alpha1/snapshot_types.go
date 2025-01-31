// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Important: Run "make generate" to regenerate code after modifying this file
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type SnapshotSpec struct {
	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This pattern is only barely more permissive than lvm VG naming rules, other than a shorter length.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9_][-a-zA-Z0-9+_.]*$"
	// +required
	VgName string `json:"vgName"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This is roughly RFC 1123 label name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	// +required
	SourceVolume string `json:"sourceVolume"`
}

const (
	SnapshotConditionAvailable = "Available"
)

type SnapshotStatus struct {
	// The generation of the spec used to produce this status.  Useful
	// as a witness when waiting for status to change.
	// +kubebuilder:validation:XValidation:rule=self>=oldSelf
	// +required
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions
	// Available: The snapshot can be sourced by volumes.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// The size of the snapshot, immutable once set.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +required
	SizeBytes int64 `json:"sizeBytes"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=snap;snaps,categories=kubesan;lv
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceVolume`,description='Volume that this snapshot was created on'
// +kubebuilder:printcolumn:name="Available",type=date,JSONPath=`.status.conditions[?(@.type=="Available")].lastTransitionTime`,description='Time since snapshot was available'
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`,description='Size of snapshot'

type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec SnapshotSpec `json:"spec"`

	// +optional
	Status SnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Snapshot{}, &SnapshotList{})
}
