// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Important: Run "make generate" to regenerate code after modifying this file
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// +kubebuilder:validation:XValidation:rule=`has(self.path)?has(oldSelf.path):true`
type NBDExportSpec struct {
	// The short name of the LV (Volume or Snapshot) to export, write-once
	// at creation.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This is roughly one RFC 1123 label name.
	// + This rule intentionally matches ThinPoolLv.ThinLvSpec.Name
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	Export string `json:"export"`

	// The "/dev/..." path of the export, write-once at creation, then
	// cleared to trigger server stop.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This rule must permit VG plus LV names. It could be written tighter
	// + to ensure the basename matches Export, but that adds CEL cost.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^(|/dev/[-_+/a-z0-9]+)$"
	// +optional
	Path string `json:"path,omitempty"`

	// The node hosting the export. Write-once at creation.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9][-.a-z0-9]*$"
	Host string `json:"host"`

	// The size of the export. Write-once at creation.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +kubebuilder:validation:Minimum=512
	// +kubebuilder:validation:MultipleOf=512
	SizeBytes int64 `json:"sizeBytes"`

	// The set of clients connecting to the export.
	// +optional
	// +listType=set
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern="^[a-z0-9][-.a-z0-9]*$"
	Clients []string `json:"clients,omitempty"`
}

const (
	NBDExportConditionAvailable = "Available"
)

type NBDExportStatus struct {
	// The generation of the spec used to produce this status.  Useful
	// as a witness when waiting for status to change.
	// +kubebuilder:validation:XValidation:rule=self>=oldSelf
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions
	// Available: The export is currently accessible
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// NBD URI for connecting to the NBD export, using IP address.
	// write-once when Conditions["Available"] is first set
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + TODO Add TLS support, which changes this to a nbds:// URI
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:Pattern="^nbd://[0-9a-f:.]+/[a-z][-a-z0-9]*$"
	URI string `json:"uri,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=nbd;nbds,categories=kubesan
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Export",type=string,JSONPath=`.spec.export`,description='LV source of the export'
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`,description='Node hosting the export'
// + TODO determine if there is a way to print a column "Clients" that displays the number of items in the .spec.clients array
// +kubebuilder:printcolumn:name="Available",type=date,JSONPath=`.status.conditions[?(@.type=="Available")].lastTransitionTime`,description='Time since export was available'
// +kubebuilder:printcolumn:name="URI",type=string,JSONPath=`.status.uri`,description='NBD URI for the export'

type NBDExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NBDExportSpec   `json:"spec,omitempty"`
	Status NBDExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type NBDExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NBDExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NBDExport{}, &NBDExportList{})
}
