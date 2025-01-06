// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"log"
	"math"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
)

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// pvName, err := getParameter("csi.storage.k8s.io/pv/name")
	// if err != nil {
	// 	return nil, err
	// }

	lvmVolumeGroup := req.Parameters["lvmVolumeGroup"]
	if lvmVolumeGroup == "" {
		return nil, status.Error(codes.InvalidArgument, "missing/empty parameter \"lvmVolumeGroup\"")
	}

	volumeMode, err := getVolumeMode(req)
	if err != nil {
		return nil, err
	}

	volumeType, err := getVolumeType(req)
	if err != nil {
		return nil, err
	}

	volumeContents, err := getVolumeContents(req)
	if err != nil {
		return nil, err
	}

	accessModes, err := getVolumeAccessModes(req)
	if err != nil {
		return nil, err
	}

	capacity, _, _, err := validateCapacity(req.CapacityRange)
	if err != nil {
		return nil, err
	}

	// Kubernetes object names are typically DNS Subdomain Names (RFC
	// 1123). Only lowercase characters are allowed.
	//
	// Converting to lowercase does not uniquely represent all possible
	// names, but it's enough to make the csi-sanity test suite work (it
	// produces uppercase names). Kubernetes itself uses the lowercase PVC
	// UUID as the name so there is no problem in practice.
	//
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/names/
	name := strings.ToLower(req.Name)

	volume := &v1alpha1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
		},
		Spec: v1alpha1.VolumeSpec{
			VgName:      lvmVolumeGroup,
			Mode:        volumeMode,
			Type:        *volumeType,
			Contents:    *volumeContents,
			AccessModes: accessModes,
			SizeBytes:   capacity,
		},
	}

	if err := s.client.Create(ctx, volume); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	err = s.client.WatchVolumeUntil(ctx, volume, func() bool {
		return meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionAvailable)
	})
	if err != nil {
		return nil, err
	}

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			CapacityBytes: capacity,
			VolumeId:      name,
			ContentSource: req.VolumeContentSource,
		},
	}

	return resp, nil
}

func getVolumeMode(req *csi.CreateVolumeRequest) (v1alpha1.VolumeMode, error) {
	mode := req.Parameters["mode"]
	if mode == "" {
		return v1alpha1.VolumeModeThin, nil
	}

	if mode != string(v1alpha1.VolumeModeThin) && mode != string(v1alpha1.VolumeModeLinear) {
		return "", status.Error(codes.InvalidArgument, "invalid volume mode")
	}

	return v1alpha1.VolumeMode(mode), nil
}

func getVolumeType(req *csi.CreateVolumeRequest) (*v1alpha1.VolumeType, error) {
	var volumeType *v1alpha1.VolumeType
	var isTypeBlock bool
	var isTypeMount bool

	for _, cap := range req.VolumeCapabilities {
		var vt v1alpha1.VolumeType

		if block := cap.GetBlock(); block != nil {
			isTypeBlock = true
			vt.Block = &v1alpha1.VolumeTypeBlock{}
		} else if mount := cap.GetMount(); mount != nil {
			isTypeMount = true
			vt.Filesystem = &v1alpha1.VolumeTypeFilesystem{
				FsType:       mount.FsType,
				MountOptions: mount.MountFlags,
			}
		} else {
			return nil, status.Error(codes.InvalidArgument, "invalid volume capabilities")
		}

		if isTypeBlock && isTypeMount {
			return nil, status.Error(codes.InvalidArgument, "volume access type cannot be both block and mount")
		}

		if volumeType == nil {
			volumeType = &v1alpha1.VolumeType{}
			*volumeType = vt
		} else if *volumeType != vt {
			return nil, status.Error(codes.InvalidArgument, "inconsistent volume capabilities")
		}
	}

	if volumeType == nil {
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	return volumeType, nil
}

func getVolumeContents(req *csi.CreateVolumeRequest) (*v1alpha1.VolumeContents, error) {
	volumeContents := &v1alpha1.VolumeContents{}

	if req.VolumeContentSource == nil {
		volumeContents.Empty = &v1alpha1.VolumeContentsEmpty{}
	} else if source := req.VolumeContentSource.GetVolume(); source != nil {
		volumeContents.CloneVolume = &v1alpha1.VolumeContentsCloneVolume{
			SourceVolume: source.VolumeId,
		}
	} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
		volumeContents.CloneSnapshot = &v1alpha1.VolumeContentsCloneSnapshot{
			SourceSnapshot: source.SnapshotId,
		}
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported volume content source")
	}

	return volumeContents, nil
}

func getVolumeAccessModes(req *csi.CreateVolumeRequest) ([]v1alpha1.VolumeAccessMode, error) {
	modes, err := kubesanslices.TryMap(req.VolumeCapabilities, getVolumeAccessMode)
	if err != nil {
		return nil, err
	}

	return kubesanslices.Deduplicate(modes), nil
}

func getVolumeAccessMode(capability *csi.VolumeCapability) (v1alpha1.VolumeAccessMode, error) {
	switch capability.AccessMode.Mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
		return v1alpha1.VolumeAccessModeSingleNodeMultiWriter, nil

	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		return v1alpha1.VolumeAccessModeSingleNodeReaderOnly, nil

	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		return v1alpha1.VolumeAccessModeMultiNodeReaderOnly, nil

	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
		return v1alpha1.VolumeAccessModeMultiNodeSingleWriter, nil

	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return v1alpha1.VolumeAccessModeMultiNodeMultiWriter, nil

	case csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER:
		return v1alpha1.VolumeAccessModeSingleNodeSingleWriter, nil

	case csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return v1alpha1.VolumeAccessModeSingleNodeMultiWriter, nil

	default:
		return "", status.Error(codes.InvalidArgument, "invalid volume access mode")
	}
}

func validateCapacity(capacityRange *csi.CapacityRange) (capacity int64, minCapacity int64, maxCapacity int64, err error) {
	if capacityRange == nil {
		// The capacity_range field is OPTIONAL in the CSI spec and the
		// plugin MAY choose an implementation-defined capacity range.
		// The csi-sanity test suite assumes the plugin chooses the
		// capacity range, so we have to pick a number here instead of
		// returning an error.
		var gigabyte int64 = 1024 * 1024 * 1024
		minCapacity = gigabyte
		maxCapacity = gigabyte
	} else {
		minCapacity = capacityRange.RequiredBytes
		maxCapacity = capacityRange.LimitBytes
	}

	if minCapacity == 0 {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "must specify minimum capacity")
	}
	if maxCapacity != 0 && maxCapacity < minCapacity {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "minimum capacity must not exceed maximum capacity")
	}

	// TODO: Check for overflow.
	capacity = (minCapacity + 511) / 512 * 512

	if maxCapacity != 0 && maxCapacity < capacity {
		return -1, -1, -1, status.Errorf(codes.InvalidArgument, "actual capacity must be a multiple of 512 bytes")
	}

	return
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// validate request

	if req.VolumeId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "must specify volume id")
	}

	// delete volume

	volume := &v1alpha1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.VolumeId,
			Namespace: config.Namespace,
		},
	}

	propagation := client.PropagationPolicy(metav1.DeletePropagationForeground)

	if err := s.client.Delete(ctx, volume, propagation); err != nil && !errors.IsNotFound(err) {
		return nil, err
	}

	// Delete() returns immediately so wait for the resource to go away

	err := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2, // exponential backoff
		Jitter:   0.1,
		Steps:    math.MaxInt,
		Cap:      10 * time.Second,
	}.DelayFunc().Until(ctx, true, false, func(ctx context.Context) (bool, error) {
		err := s.client.Get(ctx, types.NamespacedName{Name: req.VolumeId, Namespace: config.Namespace}, volume)
		if err == nil {
			log.Printf("Volume \"%v\" still exists", req.VolumeId)
			return false, nil // keep going
		} else if errors.IsNotFound(err) {
			log.Printf("Volume \"%v\" deleted", req.VolumeId)
			return true, nil // done
		} else {
			log.Printf("Volume \"%v\" Get() failed: %+v", req.VolumeId, err)
			return false, err
		}
	})
	if err != nil {
		return nil, err
	}

	// success

	resp := &csi.DeleteVolumeResponse{}

	return resp, nil
}

// func (s *ControllerServer) createVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
// 	// TODO: Reject unknown parameters in req.Parameters that *don't* start with `csi.storage.k8s.io/`.

// 	// validate request

// 	hasAccessTypeBlock := false
// 	hasAccessTypeMount := false
// 	for _, capability := range req.VolumeCapabilities {
// 		if capability.GetBlock() != nil {
// 			hasAccessTypeBlock = true
// 		} else if capability.GetMount() != nil {
// 			hasAccessTypeMount = true
// 		} else {
// 			return nil, status.Errorf(codes.InvalidArgument, "only block and mount volumes are supported")
// 		}

// 		if err := validate.ValidateVolumeCapability(capability); err != nil {
// 			return nil, err
// 		}
// 	}

// 	if hasAccessTypeBlock && hasAccessTypeMount {
// 		return nil, status.Errorf(codes.InvalidArgument, "cannot create volume with both block and mount access types")
// 	}

// 	// TODO: Cloning is currently not supported for Filesystem volumes
// 	// because mounting untrusted block devices is insecure on Linux. A
// 	// malicious file system image could trigger security bugs in the
// 	// kernel. This limitation can be removed once a way to verify that the
// 	// source volume is a Filesystem volume has been implemented.
// 	if hasAccessTypeMount && req.VolumeContentSource != nil {
// 		return nil, status.Errorf(codes.InvalidArgument, "cloning is not yet support for Filesystem volumes")
// 	}

// 	capacity, _, _, err := validateCapacity(req.CapacityRange)
// 	if err != nil {
// 		return nil, err
// 	}

// 	getParameter := func(key string) (string, error) {
// 		value := req.Parameters[key]
// 		if value == "" {
// 			return "", status.Errorf(codes.InvalidArgument, "missing/empty parameter \"%s\"", key)
// 		}
// 		return value, nil
// 	}

// 	pvName, err := getParameter("csi.storage.k8s.io/pv/name")
// 	if err != nil {
// 		return nil, err
// 	}
// 	pvcName, err := getParameter("csi.storage.k8s.io/pvc/name")
// 	if err != nil {
// 		return nil, err
// 	}
// 	pvcNamespace, err := getParameter("csi.storage.k8s.io/pvc/namespace")
// 	if err != nil {
// 		return nil, err
// 	}
// 	backingVolumeGroup, err := getParameter("backingVolumeGroup")
// 	if err != nil {
// 		return nil, err
// 	}

// 	// retrieve PVC so we can get its StorageClass

// 	pvc, err := s.BlobManager.Clientset().CoreV1().PersistentVolumeClaims(pvcNamespace).
// 		Get(ctx, pvcName, metav1.GetOptions{})
// 	if err != nil {
// 		return nil, status.Errorf(
// 			codes.Internal, "failed to get PVC \"%s\" in namespace \"%s\": %s", pvcName, pvcNamespace, err,
// 		)
// 	}

// 	// create blob

// 	blob := blobs.NewBlob(pvName, backingVolumeGroup)

// 	err = s.BlobManager.CreateBlobEmpty(ctx, blob, *pvc.Spec.StorageClassName, capacity)
// 	if err != nil {
// 		return nil, status.Errorf(codes.Internal, "failed to create empty blob \"%s\": %s", blob, err)
// 	}

// 	// populate blob

// 	if req.VolumeContentSource != nil {
// 		var sourceBlob *blobs.Blob

// 		if source := req.VolumeContentSource.GetVolume(); source != nil {
// 			volumeSourceBlob, err := blobs.BlobFromString(source.VolumeId)
// 			if err != nil {
// 				return nil, err
// 			}

// 			// Create a temporary snapshot as the source blob so
// 			// future writes to the source volume do not interfere
// 			// with populating the blob.
// 			sourceBlobName := pvName + "-createVolume-source"
// 			sourceBlob, err = s.BlobManager.CreateBlobCopy(ctx, sourceBlobName, volumeSourceBlob)
// 			if err != nil {
// 				return nil, err
// 			}

// 			defer func() {
// 				tmpErr := s.BlobManager.DeleteBlob(ctx, sourceBlob)
// 				// Failure does not affect the outcome of the request, but log the error
// 				if tmpErr != nil {
// 					log.Printf("failed to delete temporary snapshot blob %v: %v", sourceBlob, tmpErr)
// 				}
// 			}()
// 		} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
// 			sourceBlob, err = blobs.BlobFromString(source.SnapshotId)
// 		} else {
// 			return nil, status.Errorf(codes.InvalidArgument, "unsupported volume content source")
// 		}

// 		if err != nil {
// 			return nil, err
// 		}

// 		err = s.populateVolume(ctx, sourceBlob, blob)
// 		if err != nil {
// 			return nil, err
// 		}
// 	}

// 	// success

// 	resp := &csi.CreateVolumeResponse{
// 		Volume: &csi.Volume{
// 			CapacityBytes: capacity,
// 			VolumeId:      blob.String(),
// 			VolumeContext: map[string]string{},
// 			ContentSource: req.VolumeContentSource,
// 		},
// 	}
// 	return resp, nil
// }

// func (s *ControllerServer) populateVolume(ctx context.Context, sourceBlob *blobs.Blob, targetBlob *blobs.Blob) error {
// 	// TODO: Ensure that target isn't smaller than source.

// 	var ret error

// 	// attach both blobs (preferring a node where there already is a fast attachment for the source blob)

// 	cookie := fmt.Sprintf("copying-to-%s", targetBlob.Name())

// 	nodeName, sourcePathOnHost, err := s.BlobManager.AttachBlob(ctx, sourceBlob, nil, cookie)
// 	if err != nil {
// 		return status.Errorf(codes.Internal, "failed to attach blob \"%s\": %s", sourceBlob, err)
// 	}
// 	defer func() {
// 		err = s.BlobManager.DetachBlob(ctx, sourceBlob, nodeName, cookie)
// 		if err != nil && ret == nil {
// 			ret = status.Errorf(codes.Internal, "failed to detach blob \"%s\": %s", sourceBlob, err)
// 		}
// 	}()

// 	_, targetPathOnHost, err := s.BlobManager.AttachBlob(ctx, targetBlob, &nodeName, "populating")
// 	if err != nil {
// 		return status.Errorf(codes.Internal, "failed to attach blob \"%s\": %s", targetBlob, err)
// 	}
// 	defer func() {
// 		err = s.BlobManager.DetachBlob(ctx, targetBlob, nodeName, "populating")
// 		if err != nil && ret == nil {
// 			ret = status.Errorf(codes.Internal, "failed to detach blob \"%s\": %s", targetBlob, err)
// 		}
// 	}()

// 	// run population job

// 	job := &jobs.Job{
// 		Name:     fmt.Sprintf("populate-%s", targetBlob.Name()),
// 		NodeName: nodeName,
// 		Command: []string{
// 			"dd",
// 			fmt.Sprintf("if=%s", sourcePathOnHost),
// 			fmt.Sprintf("of=%s", targetPathOnHost),
// 			"bs=1M",
// 			"conv=fsync,nocreat,sparse",
// 		},
// 		ServiceAccountName: "csi-controller-plugin",
// 	}

// 	err = jobs.CreateAndRun(ctx, s.BlobManager.Clientset(), job)
// 	if err != nil {
// 		return status.Errorf(codes.Internal, "failed to populate blob \"%s\": %s", targetBlob, err)
// 	}

// 	return ret
// }

// func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
// 	// validate request

// 	if req.VolumeId == "" {
// 		return nil, status.Errorf(codes.InvalidArgument, "must specify volume id")
// 	}

// 	if req.NodeId == "" {
// 		return nil, status.Errorf(codes.InvalidArgument, "must specify node id")
// 	}

// 	// success

// 	resp := &csi.ControllerPublishVolumeResponse{}

// 	return resp, nil
// }

// func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
// 	// validate request

// 	if req.VolumeId == "" {
// 		return nil, status.Errorf(codes.InvalidArgument, "must specify volume id")
// 	}

// 	if req.NodeId == "" {
// 		return nil, status.Errorf(codes.InvalidArgument, "must specify node id")
// 	}

// 	// success

// 	resp := &csi.ControllerUnpublishVolumeResponse{}

// 	return resp, nil
// }
