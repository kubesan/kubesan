// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/hex"
	"hash/fnv"
	"log"
	"math"
	"regexp"
	"slices"
	"strconv"
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
	"gitlab.com/kubesan/kubesan/internal/csi/common/validate"
)

const (
	// TODO: It would be nice to use:
	// vgs --devicesfile=$VG --options=vg_extent_size --no-heading --no-suffix --units=b $VG
	// to determine the PE size at runtime, as well as validate that
	// the VG has properly been locked.  But doing that from the CSI
	// process requires privileges that for now we have left to only
	// the managers.  So we hard-code the default 4MiB extent size.
	defaultExtentSize = 4 * 1024 * 1024
)

var (
	pvcNamePattern = regexp.MustCompile(`^pvc-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
)

func safeName(name string) string {
	if matches := pvcNamePattern.FindStringSubmatch(name); matches != nil {
		return name
	}
	hash := fnv.New128a()
	hash.Write([]byte(name))
	return "kubesan-" + hex.EncodeToString(hash.Sum(nil))
}

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// pvName, err := getParameter("csi.storage.k8s.io/pv/name")
	// if err != nil {
	// 	return nil, err
	// }

	lvmVolumeGroup := req.Parameters["lvmVolumeGroup"]
	if lvmVolumeGroup == "" {
		return nil, status.Error(codes.InvalidArgument, "missing/empty parameter \"lvmVolumeGroup\"")
	}

	volumeMode, err := getVolumeMode(req.Parameters)
	if err != nil {
		return nil, err
	}

	volumeType, err := getVolumeType(req.VolumeCapabilities)
	if err != nil {
		return nil, err
	}

	volumeContents, err := getVolumeContents(req)
	if err != nil {
		return nil, err
	}

	accessModes, err := getVolumeAccessModes(req.VolumeCapabilities)
	if err != nil {
		return nil, err
	}

	capacity, limit, err := validateCapacity(req.CapacityRange, defaultExtentSize)
	if err != nil {
		return nil, err
	}

	// We don't advertise MODIFY_VOLUME, so mutable parameters are unexpected.
	if len(req.MutableParameters) > 0 {
		return nil, status.Error(codes.InvalidArgument, "no mutable parameters supported")
	}

	// Kubernetes object names are typically DNS Subdomain Names (RFC
	// 1123). Only lowercase characters are allowed.  When invoked by
	// the kubernetes csi-sidecar, the names match pvc-uuid, which
	// we prefer to use as-is since it is already a valid object name.
	//
	// However, CSI permits a much larger range of Unicode names,
	// other than a few control characters; since csi-sanity uses
	// such names, we map those through a hash.
	//
	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/names/
	name := safeName(req.Name)

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

	err = s.client.Create(ctx, volume)
	if errors.IsAlreadyExists(err) {
		// Check that the new request is idempotent to the existing volume
		err = s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: config.Namespace}, volume)
		if err == nil {
			if msg := validateVolume(volume, capacity, limit, volumeContents, req.VolumeCapabilities, req.Parameters); msg != "" {
				err = status.Error(codes.AlreadyExists, msg)
			}
		}
	}
	if err != nil {
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

func getVolumeMode(parameters map[string]string) (v1alpha1.VolumeMode, error) {
	mode := parameters["mode"]
	if mode == "" {
		return v1alpha1.VolumeModeThin, nil
	}

	if mode != string(v1alpha1.VolumeModeThin) && mode != string(v1alpha1.VolumeModeLinear) {
		return "", status.Error(codes.InvalidArgument, "invalid volume mode")
	}

	return v1alpha1.VolumeMode(mode), nil
}

func getVolumeType(capabilities []*csi.VolumeCapability) (*v1alpha1.VolumeType, error) {
	var volumeType *v1alpha1.VolumeType
	var isTypeBlock bool
	var isTypeMount bool

	for _, cap := range capabilities {
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
		if _, err := validate.ValidateVolumeID(source.VolumeId); err != nil {
			return nil, err
		}
		volumeContents.CloneVolume = &v1alpha1.VolumeContentsCloneVolume{
			SourceVolume: source.VolumeId,
		}
	} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
		if _, err := validate.ValidateSnapshotID(source.SnapshotId); err != nil {
			return nil, err
		}
		volumeContents.CloneSnapshot = &v1alpha1.VolumeContentsCloneSnapshot{
			SourceSnapshot: source.SnapshotId,
		}
	} else {
		return nil, status.Error(codes.InvalidArgument, "unsupported volume content source")
	}

	return volumeContents, nil
}

func getVolumeAccessModes(capabilities []*csi.VolumeCapability) ([]v1alpha1.VolumeAccessMode, error) {
	modes, err := kubesanslices.TryMap(capabilities, getVolumeAccessMode)
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

func validateCapacity(capacityRange *csi.CapacityRange, extentSize int64) (capacity, limit int64, err error) {
	var minCapacity, maxCapacity int64
	if capacityRange == nil {
		// The capacity_range field is OPTIONAL in the CSI spec and the
		// plugin MAY choose an implementation-defined capacity range.
		// The csi-sanity test suite assumes the plugin chooses the
		// capacity range, so we have to pick a number here instead of
		// returning an error.
		const gigabyte int64 = 1024 * 1024 * 1024
		minCapacity = gigabyte
		maxCapacity = gigabyte
	} else {
		minCapacity = capacityRange.RequiredBytes
		maxCapacity = capacityRange.LimitBytes
	}

	// CSI says that if capacityRange was provided, at least one of the
	// two parameters must be non-zero, and that negatives aren't allowed.
	if minCapacity == 0 && maxCapacity == 0 {
		return -1, -1, status.Error(codes.InvalidArgument, "at least one of minimum and maximum capacity must be given")
	} else if minCapacity < 0 || maxCapacity < 0 {
		return -1, -1, status.Error(codes.InvalidArgument, "capacity must be non-negative")
	}

	limit = maxCapacity
	if maxCapacity == 0 {
		maxCapacity = minCapacity + extentSize - 1
	} else if maxCapacity < minCapacity {
		return -1, -1, status.Error(codes.InvalidArgument, "minimum capacity must not exceed maximum capacity")
	}

	if minCapacity+extentSize-1 < 0 {
		return -1, -1, status.Error(codes.OutOfRange, "integer overflow when rounding capacity")
	}

	// Round capacities towards extent boundaries.
	minCapacity = (minCapacity + extentSize - 1) / extentSize * extentSize
	maxCapacity = maxCapacity / extentSize * extentSize

	if minCapacity == 0 {
		return maxCapacity, limit, nil
	} else if maxCapacity < minCapacity {
		return -1, -1, status.Errorf(codes.OutOfRange, "capacity must allow for a multiple of the extent size %d", extentSize)
	} else {
		return minCapacity, limit, nil
	}
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// validate request

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "must specify volume id")
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

// Checks whether the existing volume is compatible with a capabilities array.
// Returns "" if compatible, or a string describing an inconsistency.
func validateCapabilities(volume *v1alpha1.Volume, capabilities []*csi.VolumeCapability) string {
	if volumeType, err := getVolumeType(capabilities); err != nil || *volumeType != volume.Spec.Type {
		return "incompatible volume type"
	}

	// A subset of access modes is still okay
	accessModes, err := getVolumeAccessModes(capabilities)
	if err != nil {
		return "incomptible access modes"
	}
	for _, mode := range accessModes {
		if !slices.Contains(volume.Spec.AccessModes, mode) {
			return "incompatible access modes"
		}
	}

	return ""
}

// Checks whether the existing volume is compatible with another creation
// or validation request. Returns "" if compatible, or a string describing
// an inconsistency if incompatible.
func validateVolume(volume *v1alpha1.Volume, size int64, limit int64, source *v1alpha1.VolumeContents, capabilities []*csi.VolumeCapability, parameters map[string]string) string {
	if size > volume.Spec.SizeBytes || (limit > 0 && limit < volume.Spec.SizeBytes) {
		return "incompatible sizes"
	}
	switch {
	case source == &volume.Spec.Contents: // same pointer
	case source.Empty != nil && volume.Spec.Contents.Empty != nil: // match
	case source.CloneVolume != nil && volume.Spec.Contents.CloneVolume != nil && *source.CloneVolume == *volume.Spec.Contents.CloneVolume: // match
	case source.CloneSnapshot != nil && volume.Spec.Contents.CloneSnapshot != nil && *source.CloneSnapshot == *volume.Spec.Contents.CloneSnapshot: // match
	default:
		return "incompatible source contents"
	}

	lvmVolumeGroup := parameters["lvmVolumeGroup"]
	if lvmVolumeGroup != volume.Spec.VgName {
		return "incompatible parameter \"lvmVolumeGroup\""
	}

	if volumeMode, err := getVolumeMode(parameters); err != nil || volumeMode != volume.Spec.Mode {
		return "incompatible parameter \"mode\""
	}

	return validateCapabilities(volume, capabilities)
}

func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// validate request
	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if err := validate.ValidateVolumeContext(req.VolumeContext); err != nil {
		return nil, err
	}

	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing capability to verify")
	}

	// At present, we don't use secrets, so this should be empty.
	if len(req.Secrets) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")
	}

	// We don't advertise MODIFY_VOLUME, so mutable parameters are unexpected.
	if len(req.MutableParameters) > 0 {
		return nil, status.Error(codes.InvalidArgument, "no mutable parameters supported")
	}

	// lookup volume
	volume := &v1alpha1.Volume{}
	err = s.client.Get(ctx, namespacedName, volume)
	if errors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	if err != nil {
		return nil, err
	}

	// Check input compatibility with volume.
	if msg := validateVolume(volume, volume.Spec.SizeBytes, 0, &volume.Spec.Contents, req.VolumeCapabilities, req.Parameters); msg != "" {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: msg,
		}, nil
	}

	// Copy out only the capabilities and parameters we actually checked.
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
			Parameters:         copyKnownParameters(req.Parameters),
		},
	}, nil
}

func copyKnownParameters(parameters map[string]string) map[string]string {
	result := make(map[string]string, len(parameters))
	for key, value := range parameters {
		if key == "lvmVolumeGroup" || key == "mode" {
			result[key] = value
		}
	}
	return result
}

func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {

	// retrieve all volumes
	volumeList := &v1alpha1.VolumeList{}
	if err := s.client.Client.List(ctx, volumeList); err != nil {
		return nil, err
	}

	// validate pagination inputs

	startingToken, err := strconv.Atoi(req.StartingToken)
	if (err != nil || startingToken < 0) && req.StartingToken != "" {
		return nil, status.Error(codes.Aborted, "StartingToken does not match a token returned by an earlier call")
	} else if startingToken >= len(volumeList.Items) && len(volumeList.Items) != 0 {
		return nil, status.Error(codes.Aborted, "No volume at StartingToken")
	}

	maxEntries := int(req.MaxEntries)
	if maxEntries < 0 {
		return nil, status.Error(codes.InvalidArgument, "MaxEntries cannot be negative")
	} else if maxEntries == 0 {
		maxEntries = len(volumeList.Items)
	}

	endToken := len(volumeList.Items)
	if startingToken+maxEntries < len(volumeList.Items) {
		endToken = startingToken + maxEntries
	}

	listVolumeEntries := make([]*csi.ListVolumesResponse_Entry, 0, endToken-startingToken)

	for i := startingToken; i < endToken; i++ {
		volume := volumeList.Items[i]
		abnormal := false
		message := "Volume is operational"
		cond := meta.FindStatusCondition(volume.Status.Conditions, v1alpha1.VolumeConditionAbnormal)

		if cond != nil && cond.Status == metav1.ConditionTrue {
			abnormal = true
			message = cond.Message
		}

		entry := csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				CapacityBytes: volume.Status.SizeBytes,
				VolumeId:      volume.Spec.VgName,
			},
			Status: &csi.ListVolumesResponse_VolumeStatus{
				VolumeCondition: &csi.VolumeCondition{
					Abnormal: abnormal,
					Message:  message,
				},
				PublishedNodeIds: volume.Status.AttachedToNodes,
			},
		}

		listVolumeEntries = append(listVolumeEntries, &entry)
	}

	// nextToken is empty if end of volumeList was reached
	nextToken := strconv.Itoa(endToken)
	if endToken == len(volumeList.Items) {
		nextToken = ""
	}

	return &csi.ListVolumesResponse{
		Entries:   listVolumeEntries,
		NextToken: nextToken,
	}, nil
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
