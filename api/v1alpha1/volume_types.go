// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
)

// Important: Run "make generate" to regenerate code after modifying this file
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type VolumeSpec struct {
	// Should be set from creation and never updated.
	// +kubebuilder:validation:XValidation:rule=oldSelf==self
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
	AccessModes []VolumeAccessMode `json:"accessModes"`

	// Must be positive and a multiple of 512. May be updated at will, but the actual size will only ever increase.
	// +kubebuilder:validation:Minimum=512
	// +kubebuilder:validation:MultipleOf=512
	SizeBytes int64 `json:"sizeBytes"`

	// May be updated at will.
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
	FsType       string   `json:"fsType"`
	MountOptions []string `json:"mountOptions,omitempty"`
}

type VolumeContents struct {
	Empty         *VolumeContentsEmpty         `json:"empty,omitempty"`
	CloneVolume   *VolumeContentsCloneVolume   `json:"cloneVolume,omitempty"`
	CloneSnapshot *VolumeContentsCloneSnapshot `json:"cloneSnapshot,omitempty"`
}

type VolumeContentsEmpty struct {
}

type VolumeContentsCloneVolume struct {
	SourceVolume string `json:"sourceVolume"`
}

type VolumeContentsCloneSnapshot struct {
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

type VolumeStatus struct {
	// Conditions
	// Available: The LVM volume has been created
	// DataSourceCompleted: Any data source has been copied into the LVM,
	// so that it is now ready to be attached to nodes
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []conditionsv1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Reflects the current size of the volume.
	// +kubebuilder:validation:XValidation:rule=oldSelf<=self
	SizeBytes int64 `json:"sizeBytes"`

	// Reflects the nodes to which the volume is attached.
	AttachedToNodes []string `json:"attachedToNodes,omitempty"`

	// The path at which the volume is available on nodes to which it is attached.
	// + TODO does this have to be in Status, or can it be reliably generated/probed where needed?
	Path *string `json:"path,omitempty"`
}

func (v *VolumeStatus) IsAttachedToNode(node string) bool {
	return slices.Contains(v.AttachedToNodes, node)
}

func (v *VolumeStatus) GetPath() string {
	if v.Path == nil {
		return ""
	} else {
		return *v.Path
	}
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="VG",type=string,JSONPath=`.spec.vgName`,description='VG owning the volume'
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`,description='Path to volume in nodes where it is active',priority=1
// +kubebuilder:printcolumn:name="Available",type=date,JSONPath=`.status.conditions[?(@.type=="Available")].lastTransitionTime`,description='Time since volume was available'
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
