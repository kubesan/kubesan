// SPDX-License-Identifier: Apache-2.0

// CSI validation logic shared by the controller and node plugins
package validate

import (
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"gitlab.com/kubesan/kubesan/internal/common/config"
)

// Validate a VolumeID or SnapshotID. As a convenience, provide a
// NamespacedName on success, since most callers will eventually want
// to call client.Get() to look up the k8s object corresponding to id.
func validateID(id string, category string) (types.NamespacedName, error) {
	if id == "" {
		return types.NamespacedName{}, status.Error(codes.InvalidArgument, category+" id cannot be empty")
	}
	// Although we do not check whether a volume/snapshot exists
	// with the given id, we can at least check that the id
	// forms a valid k8s object name.
	errs := validation.IsDNS1123Label(id)
	if len(errs) > 0 {
		return types.NamespacedName{}, status.Errorf(codes.InvalidArgument, "%s id is not a valid RFC 1123 label: %s", category, strings.Join(errs, ", "))
	}

	return types.NamespacedName{Name: id, Namespace: config.Namespace}, nil
}

// Validate a VolumeID (as returned by CreateVolume).
func ValidateVolumeID(id string) (types.NamespacedName, error) {
	return validateID(id, "volume")
}

// Validate a SnapshotID (as returned by CreateSnapshot).
func ValidateSnapshotID(id string) (types.NamespacedName, error) {
	return validateID(id, "snapshot")
}

// Validate a Volume Context.
func ValidateVolumeContext(context map[string]string) error {
	// Since VolumeCreate currently does not pass back a context, we
	// never expect a caller to pass us a context.
	if len(context) > 0 {
		return status.Error(codes.InvalidArgument, "unexpected volume context")
	}
	return nil
}

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
