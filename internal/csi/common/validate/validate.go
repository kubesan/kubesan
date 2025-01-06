// SPDX-License-Identifier: Apache-2.0

// CSI validation logic shared by the controller and node plugins
package validate

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ValidateVolumeCapability(capability *csi.VolumeCapability) error {
	if mount := capability.GetMount(); mount != nil {
		return validateVolumeCapabilityMount(capability)
	} else if capability.GetBlock() == nil {
		return status.Error(codes.InvalidArgument, "expected a block or mount volume")
	}
	return nil
}

func validateVolumeCapabilityMount(capability *csi.VolumeCapability) error {
	// Reject multi-node access modes
	accessMode := capability.GetAccessMode().GetMode()
	if accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
		accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY &&
		accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER &&
		accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER {
		return status.Errorf(codes.InvalidArgument, "Filesystem volumes only support single-node access modes (got %d)", accessMode)
	}
	return nil
}
