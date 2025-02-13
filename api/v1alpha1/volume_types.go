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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9_][-a-zA-Z0-9+_.]*$"
	// +required
	VgName string `json:"vgName"`

	// Should be set from creation and never updated, if available.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Binding string `json:"binding,omitempty"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +kubebuilder:validation:Enum=Thin;Linear
	// +required
	Mode VolumeMode `json:"mode"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +kubebuilder:validation:Enum=Full;UnsafeFast
	// +default:value="Full"
	// +optional
	WipePolicy VolumeWipePolicy `json:"wipePolicy,omitempty"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +required
	Type VolumeType `json:"type"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +required
	Contents VolumeContents `json:"contents"`

	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
	// +listType=set
	// +required
	AccessModes []VolumeAccessMode `json:"accessModes"`

	// Must be positive and a multiple of 512. May be updated at will, but the actual size will only ever increase.
	// +kubebuilder:validation:Minimum=512
	// +kubebuilder:validation:MultipleOf=512
	// +required
	SizeBytes int64 `json:"sizeBytes"`

	// May be updated at will.
	// +listType=set
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern="^[a-z0-9][-.a-z0-9]*$"
	// +optional
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
type VolumeWipePolicy string

const (
	VolumeModeThin   VolumeMode = "Thin"
	VolumeModeLinear VolumeMode = "Linear"

	VolumeWipePolicyFull       VolumeWipePolicy = "Full"
	VolumeWipePolicyUnsafeFast VolumeWipePolicy = "UnsafeFast"
)

// + TODO make this a proper discriminated union
type VolumeType struct {
	Block      *VolumeTypeBlock      `json:"block,omitempty"`
	Filesystem *VolumeTypeFilesystem `json:"filesystem,omitempty"`
}

type VolumeTypeBlock struct {
}

type VolumeTypeFilesystem struct {
	// +required
	FsType string `json:"fsType"`

	// +listType=atomic
	// +optional
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
	// +required
	ContentsType string `json:"contentsType"`

	// +optional
	CloneVolume *VolumeContentsCloneVolume `json:"cloneVolume,omitempty"`

	// +optional
	CloneSnapshot *VolumeContentsCloneSnapshot `json:"cloneSnapshot,omitempty"`
}

type VolumeContentsCloneVolume struct {
	// + This is roughly one RFC 1123 label name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	// +required
	SourceVolume string `json:"sourceVolume"`
}

type VolumeContentsCloneSnapshot struct {
	// + This is roughly one RFC 1123 label name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	// +required
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
	// +required
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions
	// LvCreated: An empty LVM Logical Volume has been created but may
	// still require population from a data source
	// DataSourceCompleted: Data has been populated from the data source
	// Available: The LVM volume has been created
	// DataSourceCompleted: Any data source has been copied into the LVM,
	// so that it is now ready to be attached to nodes
	// Abnormal: Indicates the health of a volume
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Reflects the current size of the volume.
	// +kubebuilder:validation:XValidation:rule=oldSelf<=self
	// +required
	SizeBytes int64 `json:"sizeBytes"`

	// Reflects the nodes to which the volume is attached.
	// +listType=set
	// + This rule must permit RFC 1123 DNS subdomain names.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern="^[a-z0-9][-.a-z0-9]*$"
	// +optional
	AttachedToNodes []string `json:"attachedToNodes,omitempty"`

	// The path at which the volume is available on nodes to which it is attached.
	// + This rule must permit VG plus LV names. It could be written tighter
	// + to ensure the basename matches our LV rules, but that adds CEL cost.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^(|/dev/[-_+/a-z0-9]+)$"
	// +optional
	Path string `json:"path,omitempty"`

	// The name of any NBDExport serving this volume.
	// + This is roughly two RFC 1123 label names.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern="^[a-z0-9][-a-z0-9]*$"
	// +optional
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
// +kubebuilder:printcolumn:name="Binding",type=string,JSONPath=`.spec.binding`,description='PVC object bound to this volume',priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description='Time since resource was created'
// +kubebuilder:printcolumn:name="Available",type=date,JSONPath=`.status.conditions[?(@.type=="Available")].lastTransitionTime`,description='Time since last volume creation or expansion'
// +kubebuilder:printcolumn:name="Primary Node",type=string,JSONPath=`.status.attachedToNodes[0]`,description='Primary node where volume is currently active'
// + TODO determine if there is a way to print a column "Active Nodes" that displays the number of items in the .status.attachedToNodes array
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`,description='Size of volume'
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`,description='Mode of volume (thin or linear)',priority=2
// +kubebuilder:printcolumn:name="FSType",type=string,JSONPath=`.spec.type.filesystem.fsType`,description='Filesystem type (blank if block)',priority=2
// +kubebuilder:selectablefield:JSONPath=`.spec.vgName`
// +kubebuilder:selectablefield:JSONPath=`.spec.mode`

// Volume is the Schema for the volumes API
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec VolumeSpec `json:"spec"`

	// +optional
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
