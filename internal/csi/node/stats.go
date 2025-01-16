// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/csi/common/validate"
)

// Returns statistics for a given volume from this node's perspective.
func (s *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {

	// input validation
	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if req.VolumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume Path not provided")
	}

	if _, err := os.Stat(req.VolumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Error(codes.NotFound, "Failed to find a volume at "+req.VolumePath)
		}
		return nil, err
	}

	// retrieve the volume
	volume := &v1alpha1.Volume{}
	if err := s.client.Get(ctx, namespacedName, volume); err != nil {
		if errors.IsNotFound(err) {
			return nil, status.Error(codes.NotFound, req.VolumeId+" does not exist")
		}
		return nil, err
	}

	abnormal := false
	message := "Volume is operational"
	cond := meta.FindStatusCondition(volume.Status.Conditions, v1alpha1.VolumeConditionAbnormal)
	if cond != nil && cond.Status == metav1.ConditionTrue {
		abnormal = true
		message = cond.Message
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Total: volume.Status.SizeBytes,
				Unit:  csi.VolumeUsage_BYTES,
			},
		},
		VolumeCondition: &csi.VolumeCondition{
			Abnormal: abnormal,
			Message:  message,
		},
	}, nil
}
