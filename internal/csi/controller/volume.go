// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// The easiest way to reject unknown parameters is to see if a
	// copy of only known parameters is the same length.
	knownParameters := copyKnownParameters(req.Parameters)
	if len(req.Parameters) > len(knownParameters) {
		return nil, status.Error(codes.InvalidArgument, "unexpected parameters")
	}

	lvmVolumeGroup := req.Parameters["lvmVolumeGroup"]
	if lvmVolumeGroup == "" {
		return nil, status.Error(codes.InvalidArgument, "missing/empty parameter \"lvmVolumeGroup\"")
	}

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "must specify volume name")
	}

	volumeMode, err := getVolumeMode(req.Parameters)
	if err != nil {
		return nil, err
	}

	wipePolicy, err := getWipePolicy(req.Parameters)
	if err != nil {
		return nil, err
	}

	volumeType, err := getVolumeType(req.VolumeCapabilities)
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

	volumeContents, err := s.getVolumeContents(ctx, req, lvmVolumeGroup, limit, volumeType)
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
	name := validate.SafeName(req.Name, "pvc")
	binding := req.Name
	if binding == name {
		pvcNamespace := req.Parameters["csi.storage.k8s.io/pvc/namespace"]
		pvcName := req.Parameters["csi.storage.k8s.io/pvc/name"]
		binding = pvcNamespace + "/" + pvcName
	}

	volume := &v1alpha1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
			Labels:    config.CommonLabels,
		},
		Spec: v1alpha1.VolumeSpec{
			VgName:      lvmVolumeGroup,
			Binding:     binding,
			Mode:        volumeMode,
			WipePolicy:  wipePolicy,
			Type:        *volumeType,
			Contents:    *volumeContents,
			AccessModes: accessModes,
			SizeBytes:   capacity,
		},
	}
	controllerutil.AddFinalizer(volume, config.Finalizer)

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

func getWipePolicy(parameters map[string]string) (v1alpha1.VolumeWipePolicy, error) {
	switch policy := parameters["wipePolicy"]; policy {
	case "":
		fallthrough
	case string(v1alpha1.VolumeWipePolicyFull):
		return v1alpha1.VolumeWipePolicyFull, nil
	case string(v1alpha1.VolumeWipePolicyUnsafeFast):
		return v1alpha1.VolumeWipePolicyUnsafeFast, nil
	}
	return "", status.Error(codes.InvalidArgument, "invalid volume wipe policy")
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

// Determine what to populate the volume with, and ensure that any source
// exists in vgName, is not larger than any non-zero limit, and that we
// are not attempting to convert a block into a filesystem.
func (s *ControllerServer) getVolumeContents(ctx context.Context, req *csi.CreateVolumeRequest, vgName string, limit int64, volumeType *v1alpha1.VolumeType) (*v1alpha1.VolumeContents, error) {
	volumeContents := &v1alpha1.VolumeContents{}

	if req.VolumeContentSource == nil {
		// Empty source
		volumeContents.ContentsType = v1alpha1.VolumeContentsTypeEmpty

	} else if source := req.VolumeContentSource.GetVolume(); source != nil {
		// Volume source
		name, err := validate.ValidateVolumeID(source.VolumeId)
		if err != nil {
			return nil, err
		}
		volume := &v1alpha1.Volume{}
		err = s.client.Get(ctx, name, volume)
		// TODO: Once we get Conditions["Error"] reporting working,
		// it may be nicer to do these checks only in the manager
		// code, instead of duplicating them here as fail-fast checks.
		switch {
		case errors.IsNotFound(err):
			return nil, status.Error(codes.NotFound, "source volume does not exist")

		case err != nil:
			return nil, status.Errorf(codes.InvalidArgument, "unable to inspect source volume: %v", err)

		case volume.DeletionTimestamp != nil:
			return nil, status.Error(codes.NotFound, "source volume is marked for deletion")

		case volume.Spec.VgName != vgName:
			// TODO We could possibly relax this for multiple VGs that share a common node in topology
			return nil, status.Error(codes.InvalidArgument, "source volume does not live in same volume group as destination")

		case volumeType.Filesystem != nil:
			return nil, status.Error(codes.InvalidArgument, "cannot create filesystem volume from a block device source")

		case volume.Spec.Type.Filesystem != nil:
			// TODO For now, we simply refuse to support cloning
			// a block from a filesystem. It might be possible to
			// support this in the future, though.
			return nil, status.Error(codes.InvalidArgument, "cannot create block device from a filesystem source")

		case volume.Spec.Mode == v1alpha1.VolumeModeLinear:
			// Cloning a linear volume is not possible.
			return nil, status.Error(codes.InvalidArgument, "cannot clone from a linear volume")

		case !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionAvailable):
			return nil, status.Error(codes.NotFound, "source volume is not ready yet")

		case limit > 0 && volume.Status.SizeBytes > limit:
			return nil, status.Errorf(codes.OutOfRange, "destination capacity is insufficient to accommodate source size %d", volume.Status.SizeBytes)
		}
		volumeContents.ContentsType = v1alpha1.VolumeContentsTypeCloneVolume
		volumeContents.CloneVolume = &v1alpha1.VolumeContentsCloneVolume{
			SourceVolume: source.VolumeId,
		}

	} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
		// Snapshot source
		name, err := validate.ValidateSnapshotID(source.SnapshotId)
		if err != nil {
			return nil, err
		}
		snapshot := &v1alpha1.Snapshot{}
		err = s.client.Get(ctx, name, snapshot)
		switch {
		case errors.IsNotFound(err):
			return nil, status.Error(codes.NotFound, "source snapshot does not exist")

		case err != nil:
			return nil, status.Errorf(codes.InvalidArgument, "unable to inspect source snapshot: %v", err)

		case snapshot.DeletionTimestamp != nil:
			return nil, status.Error(codes.NotFound, "source snapshot is marked for deletion")

		case snapshot.Spec.VgName != vgName:
			// TODO We could possibly relax this for multiple VGs that share a common node in topology
			return nil, status.Error(codes.InvalidArgument, "source snapshot does not live in same volume group as destination")

		case volumeType.Filesystem != nil:
			// TODO This is true as long as Snapshots can only be created from block devices
			return nil, status.Error(codes.InvalidArgument, "cannot create filesystem volume from a block device source")

		case !meta.IsStatusConditionTrue(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable):
			return nil, status.Error(codes.NotFound, "source snapshot is not ready yet")

		case limit > 0 && snapshot.Status.SizeBytes > limit:
			return nil, status.Errorf(codes.OutOfRange, "destination capacity is insufficient to accommodate source size %d", snapshot.Status.SizeBytes)
		}
		volumeContents.ContentsType = v1alpha1.VolumeContentsTypeCloneSnapshot
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

	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	// At present, we don't use secrets, so this should be empty.
	if len(req.Secrets) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")
	}

	// ensure volume is not currently staged

	volume := &v1alpha1.Volume{}
	err = s.client.Get(ctx, namespacedName, volume)
	if errors.IsNotFound(err) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(volume.Spec.AttachToNodes) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "volume is still staged")
	}

	// delete volume

	if err := s.client.DeleteAndConfirm(ctx, volume); err != nil {
		return nil, err
	}

	// success

	resp := &csi.DeleteVolumeResponse{}

	return resp, nil
}

// Checks whether the existing volume is compatible with a capabilities array.
// Returns "" if compatible, or a string describing an inconsistency.
func validateCapabilities(volume *v1alpha1.Volume, capabilities []*csi.VolumeCapability) string {
	volumeType, err := getVolumeType(capabilities)
	switch {
	case err != nil:
		return "incompatible volume type: " + err.Error()
	case volumeType.Block != nil && volume.Spec.Type.Filesystem != nil:
		return "incompatible volume type: cannot change block to filesystem"
	case volumeType.Filesystem != nil && volume.Spec.Type.Block != nil:
		return "incompatible volume type: cannot change filesystem to block"
	case volumeType.Filesystem != nil && volume.Spec.Type.Filesystem != nil && !reflect.DeepEqual(volumeType.Filesystem, volume.Spec.Type.Filesystem):
		return fmt.Sprintf("incompatible volume type: filesystem mismatch %v vs. %v", volumeType.Filesystem, volume.Spec.Type.Filesystem)
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
	if volume.DeletionTimestamp != nil {
		return "volume deletion is already in progress"
	}

	if size > volume.Spec.SizeBytes || (limit > 0 && limit < volume.Spec.SizeBytes) {
		return "incompatible sizes"
	}
	switch {
	case source == &volume.Spec.Contents: // same pointer
	case source.ContentsType != volume.Spec.Contents.ContentsType:
		return "incompatible source contents"
	case source.ContentsType == v1alpha1.VolumeContentsTypeEmpty: // match
	case source.ContentsType == v1alpha1.VolumeContentsTypeCloneVolume && source.CloneVolume != nil && volume.Spec.Contents.CloneVolume != nil && *source.CloneVolume == *volume.Spec.Contents.CloneVolume: // match
	case source.ContentsType == v1alpha1.VolumeContentsTypeCloneSnapshot && source.CloneSnapshot != nil && volume.Spec.Contents.CloneSnapshot != nil && *source.CloneSnapshot == *volume.Spec.Contents.CloneSnapshot: // match
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

	if wipePolicy, err := getWipePolicy(parameters); err != nil || wipePolicy != volume.Spec.WipePolicy {
		return "incompatible parameter \"wipePolicy\""
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
		switch key {
		default:
			continue
		case "lvmVolumeGroup":
		case "mode":
		case "wipePolicy":
		case "csi.storage.k8s.io/pvc/name":
		case "csi.storage.k8s.io/pvc/namespace":
		case "csi.storage.k8s.io/pv/name":
		}
		result[key] = value
	}
	return result
}

func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {

	// retrieve all volumes
	volumeList := &v1alpha1.VolumeList{}
	if err := s.client.List(ctx, volumeList); err != nil {
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
				VolumeId:      volume.Name,
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

func (s *ControllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {

	// input validation
	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
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

	return &csi.ControllerGetVolumeResponse{
		Volume: &csi.Volume{
			CapacityBytes: volume.Status.SizeBytes,
			VolumeId:      volume.Name,
		},
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: abnormal,
				Message:  message,
			},
			PublishedNodeIds: volume.Status.AttachedToNodes,
		},
	}, nil
}

// Expand the size of a volume.
func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	// validate request
	namespacedName, err := validate.ValidateVolumeID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	capacity, _, err := validateCapacity(req.CapacityRange, defaultExtentSize)
	if err != nil {
		return nil, err
	}

	// At present, we don't use secrets, so this should be empty.
	if len(req.Secrets) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")
	}

	if req.VolumeCapability != nil {
		if err := validate.ValidateVolumeCapability(req.VolumeCapability); err != nil {
			return nil, err
		}
	}

	// lookup volume
	volume := &v1alpha1.Volume{}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.client.Get(ctx, namespacedName, volume); err != nil {
			if errors.IsNotFound(err) {
				return status.Error(codes.NotFound, "volume does not exist")
			}
			return err
		}

		if req.VolumeCapability != nil {
			if msg := validateCapabilities(volume, []*csi.VolumeCapability{req.VolumeCapability}); msg != "" {
				return status.Error(codes.InvalidArgument, msg)
			}
		}

		if volume.Spec.Mode == v1alpha1.VolumeModeLinear && len(volume.Spec.AttachToNodes)+len(volume.Status.AttachedToNodes) > 0 {
			return status.Error(codes.FailedPrecondition, "linear volume can only be expanded offline")
		}

		if volume.Spec.Type.Filesystem != nil {
			return status.Error(codes.Unimplemented, "filesystem expansion not implemented yet")
		}

		if capacity > volume.Spec.SizeBytes {
			volume.Spec.SizeBytes = capacity
			if err := s.client.Update(ctx, volume); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = s.client.WatchVolumeUntil(ctx, volume, func() bool {
		return capacity <= volume.Status.SizeBytes
	})
	if err != nil {
		return nil, err
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         volume.Status.SizeBytes,
		NodeExpansionRequired: false,
	}, nil
}
