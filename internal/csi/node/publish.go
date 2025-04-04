// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"os"
	"slices"

	"k8s.io/mount-utils"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/csi/common/validate"
)

func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	// validate request

	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if err := validate.ValidateVolumeContext(req.PublishContext); err != nil {
		return nil, err
	}

	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "must specify target path")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "must specify volume capability")
	}

	if err := validate.ValidateVolumeCapability(req.VolumeCapability); err != nil {
		return nil, err
	}

	// Check that volume still exists

	volume := &v1alpha1.Volume{}
	err = s.client.Get(ctx, namespacedName, volume)
	if k8serrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	if err != nil {
		return nil, err
	}

	if !slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName) {
		return nil, status.Error(codes.FailedPrecondition, "volume is not staged on node")
	}

	if volume.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "volume deletion is already in progress")
	}
	// TODO check that volume_capability and readonly are compatible

	// Proceed with publishing

	if req.VolumeCapability.GetMount() != nil {
		// create bind mount for Filesystem volumes

		err := os.MkdirAll(req.TargetPath, 0750)
		if err != nil {
			return nil, err
		}

		err = s.mounter.Mount(req.StagingTargetPath, req.TargetPath, "none", []string{"bind"})
		if err != nil {
			_ = os.Remove(req.TargetPath)
			return nil, err
		}
	} else {
		// create symlink to device for Kubernetes (replace if there is a file, empty dir, or symlink in its place;
		// Kubernetes places an empty directory at the path where block volumes should be staged/published)

		err := os.Remove(req.TargetPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		err = os.Symlink(req.StagingTargetPath, req.TargetPath)
		if err != nil {
			return nil, err
		}
	}

	// success

	resp := &csi.NodePublishVolumeResponse{}

	return resp, nil
}

func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	// validate request

	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "must specify target path")
	}

	// Proceed with unmounting

	isMountPoint, err := s.mounter.IsMountPoint(req.TargetPath)
	if err == nil && isMountPoint {
		// remove bind mount created by NodePublishVolume for a Filesystem volume

		err := mount.CleanupMountPoint(req.TargetPath, s.mounter, true)
		if err != nil {
			return nil, err
		}
	} else {
		// remove symlink created by NodePublishVolume for a Block volume

		err := os.Remove(req.TargetPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	// Check that volume still exists

	volume := &v1alpha1.Volume{}
	err = s.client.Get(ctx, namespacedName, volume)
	if k8serrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	if err != nil {
		return nil, err
	}

	// success

	resp := &csi.NodeUnpublishVolumeResponse{}

	return resp, nil
}
