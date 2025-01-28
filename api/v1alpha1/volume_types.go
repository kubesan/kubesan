// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Important: Run "make generate" to regenerate code after modifying this file
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type VolumeSpec struct {
	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// + This pattern is only barely more permissive than lvm VG naming rules, other than a shorter length.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9_][-a-zA-Z0-9+_.]*$"
	VgName string `json:"vgName"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +kubebuilder:validation:Enum=Thin;Linear
	Mode VolumeMode `json:"mode"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	Type VolumeType `json:"type"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	Contents VolumeContents `json:"contents"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +listType=set
	AccessModes []VolumeAccessMode `json:"accessModes"`

	// Must be positive and a multiple of 512. May be updated at will, but the actual size will only ever increase.
	// +kubebuilder:validation:Minimum=512
	// +kubebuilder:validation:MultipleOf=512
	SizeBytes int64 `json:"sizeBytes"`

	// May be updated at will.
	// +listType=set
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern="^[a-z0-9][-.a-z0-9]*$"
	AttachToNodes []string `json:"attachToNodes,omitempty"`
}

func (v *VolumeSpec) ReadOnly() bool {
	for _, mode := range v.AccessModes {
		switch mode {
		case VolumeAccessModeSingleNodeSingleWriter, VolumeAccessModeSingleNodeMultiWriter,
			VolumeAccessModeMultiNodeSingleWriter, VolumeAccessModeMultiNodeMultiWriter:
			return false

		case VolumeAccessModeSingleNodeReaderOnly,
			VolumeAccessModeMultiNodeReaderOnly:
		}
	}

	return true
}

type VolumeMode string

const (
	VolumeModeThin   VolumeMode = "Thin"
	VolumeModeLinear VolumeMode = "Linear"
)

type VolumeType struct {
	Block      *VolumeTypeBlock      `json:"block,omitempty"`
	Filesystem *VolumeTypeFilesystem `json:"filesystem,omitempty"`
}

type VolumeTypeBlock struct {
}

type VolumeTypeFilesystem struct {
	FsType string `json:"fsType"`

	// +listType=atomic
	MountOptions []string `json:"mountOptions,omitempty"`
}

const (
	VolumeContentsTypeEmpty         = "Empty"
	VolumeContentsTypeCloneVolume   = "CloneVolume"
	VolumeContentsTypeCloneSnapshot = "CloneSnapshot"
)

// +union
type VolumeContents struct {
	// +unionDiscriminator
	// +kubebuilder:validation:Enum:="Empty";"CloneVolume";"CloneSnapshot"
	// +kubebuilder:validation:Required
	ContentsType string `json:"contentsType"`

	// +optional
	CloneVolume *VolumeContentsCloneVolume `json:"cloneVolume,omitempty"`

	// +optional
	CloneSnapshot *VolumeContentsCloneSnapshot `json:"cloneSnapshot,omitempty"`
}

type VolumeContentsCloneVolume struct {
	// + This is roughly one RFC 1123 label name.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	SourceVolume string `json:"sourceVolume"`
}

type VolumeContentsCloneSnapshot struct {
	// + This is roughly one RFC 1123 label name.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	SourceSnapshot string `json:"sourceSnapshot"`
}

type VolumeAccessMode string

const (
	VolumeAccessModeSingleNodeReaderOnly   VolumeAccessMode = "SingleNodeReaderOnly"
	VolumeAccessModeSingleNodeSingleWriter VolumeAccessMode = "SingleNodeSingleWriter"
	VolumeAccessModeSingleNodeMultiWriter  VolumeAccessMode = "SingleNodeMultiWriter"

	VolumeAccessModeMultiNodeReaderOnly   VolumeAccessMode = "MultiNodeReaderOnly"
	VolumeAccessModeMultiNodeSingleWriter VolumeAccessMode = "MultiNodeSingleWriter"
	VolumeAccessModeMultiNodeMultiWriter  VolumeAccessMode = "MultiNodeMultiWriter"
)

const (
	VolumeConditionLvCreated           = "LvCreated"
	VolumeConditionDataSourceCompleted = "DataSourceCompleted"
	VolumeConditionAvailable           = "Available"
	VolumeConditionAbnormal            = "Abnormal"
)

type VolumeStatus struct {
	// The generation of the spec used to produce this status.  Useful
	// as a witness when waiting for status to change.
	// +kubebuilder:validation:XValidation:rule=self>=oldSelf
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions
	// LvCreated: An empty LVM Logical Volume has been created but may
	// still require population from a data source
	// DataSourceCompleted: Data has been populated from the data source
	// Available: The LVM volume has been created
	// DataSourceCompleted: Any data source has been copied into the LVM,
	// so that it is now ready to be attached to nodes
	// Abnormal: Indicates the health of a volume
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Reflects the current size of the volume.
	// +kubebuilder:validation:XValidation:rule=oldSelf<=self
	SizeBytes int64 `json:"sizeBytes"`

	// Reflects the nodes to which the volume is attached.
	// +listType=set
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern="^[a-z0-9][-.a-z0-9]*$"
	AttachedToNodes []string `json:"attachedToNodes,omitempty"`

	// The path at which the volume is available on nodes to which it is attached.
	// + TODO does this have to be in Status, or can it be reliably generated/probed where needed?
	// + This rule must permit VG plus LV names. It could be written tighter
	// + to ensure the basename matches our LV rules, but that adds CEL cost.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^(|/dev/[-_+/a-z0-9]+)$"
	Path string `json:"path,omitempty"`

	// The name of any NBDExport serving this volume.
	// + This is roughly two RFC 1123 label names.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	NBDExport string `json:"nbdExport,omitempty"`
}

func (v *VolumeStatus) IsAttachedToNode(node string) bool {
	return slices.Contains(v.AttachedToNodes, node)
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=vol;vols,categories=kubesan;lv
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="VG",type=string,JSONPath=`.spec.vgName`,description='VG owning the volume'
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`,description='Path to volume in nodes where it is active',priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description='Time since resource was created'
// +kubebuilder:printcolumn:name="Available",type=date,JSONPath=`.status.conditions[?(@.type=="Available")].lastTransitionTime`,description='Time since last volume creation or expansion'
// +kubebuilder:printcolumn:name="Primary Node",type=string,JSONPath=`.status.attachedToNodes[0]`,description='Primary node where volume is currently active'
// + TODO determine if there is a way to print a column "Active Nodes" that displays the number of items in the .status.attachedToNodes array
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`,description='Size of volume'
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`,description='Mode of volume (thin or linear)',priority=2
// +kubebuilder:printcolumn:name="FSType",type=string,JSONPath=`.spec.type.filesystem.fsType`,description='Filesystem type (blank if block)',priority=2

// Volume is the Schema for the volumes API
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeSpec   `json:"spec,omitempty"`
	Status VolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeList contains a list of Volume
type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Volume{}, &VolumeList{})
}
