// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// Note that passing --extra-create-metadata in
	// controller-plugin.yaml would give us access to the
	// PersistentVolumeClaim object, but our current design does
	// not need it.
	// See https://gitlab.com/kubesan/kubesan/-/issues/105.
	// pvName, err := getParameter("csi.storage.k8s.io/pv/name")
	// if err != nil {
	// 	return nil, err
	// }

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
	name := validate.SafeName(req.Name, "pvc")

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
		volumeContents.ContentsType = v1alpha1.VolumeContentsTypeEmpty
	} else if source := req.VolumeContentSource.GetVolume(); source != nil {
		if _, err := validate.ValidateVolumeID(source.VolumeId); err != nil {
			return nil, err
		}
		volumeContents.ContentsType = v1alpha1.VolumeContentsTypeCloneVolume
		volumeContents.CloneVolume = &v1alpha1.VolumeContentsCloneVolume{
			SourceVolume: source.VolumeId,
		}
	} else if source := req.VolumeContentSource.GetSnapshot(); source != nil {
		if _, err := validate.ValidateSnapshotID(source.SnapshotId); err != nil {
			return nil, err
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

func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {

	// retrieve all snapshots
	snapshotList := &v1alpha1.SnapshotList{}
	if err := s.client.Client.List(ctx, snapshotList); err != nil {
		return nil, err
	}

	// validate pagination inputs
	startingToken, err := strconv.Atoi(req.StartingToken)
	if (err != nil || startingToken < 0) && req.StartingToken != "" {
		return nil, status.Error(codes.Aborted, "StartingToken does not match a token returned by an earlier call")
	} else if startingToken >= len(snapshotList.Items) && len(snapshotList.Items) != 0 {
		return nil, status.Error(codes.Aborted, "No snapshot at StartingToken")
	}

	maxEntries := int(req.MaxEntries)
	if maxEntries < 0 {
		return nil, status.Error(codes.InvalidArgument, "MaxEntries cannot be negative")
	} else if maxEntries == 0 {
		maxEntries = len(snapshotList.Items)
	}

	// At present, we don't use secrets, so this should be empty.
	if len(req.Secrets) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")
	}

	// return snapshot with specific snapshot ID or empty entry list if not found
	if req.SnapshotId != "" {
		if req.StartingToken != "" {
			return nil, status.Error(codes.Aborted, "StartingToken should be empty if SnapshotId is specified")
		}

		namespacedName, err := validate.ValidateSnapshotID(req.SnapshotId)
		if err != nil {
			return nil, err
		}

		// retrieve snapshot with SnapshotId
		snapshot := &v1alpha1.Snapshot{}
		if err := s.client.Get(ctx, namespacedName, snapshot); err != nil {
			if errors.IsNotFound(err) {
				return &csi.ListSnapshotsResponse{}, nil
			}
			return nil, err
		}

		entry := getListSnapshotEntry(snapshot, req.SourceVolumeId)
		if entry == nil {
			return nil, status.Error(codes.NotFound, "Snapshot not available")
		}

		return &csi.ListSnapshotsResponse{
			Entries: []*csi.ListSnapshotsResponse_Entry{
				entry,
			},
		}, nil
	}

	// sort by SourceVolume to ensure consistency between calls
	slices.SortFunc(snapshotList.Items, func(snapA, snapB v1alpha1.Snapshot) int {
		cmp := strings.Compare(snapA.Spec.SourceVolume, snapB.Spec.SourceVolume)
		if cmp == 0 {
			cmp = strings.Compare(snapA.Name, snapB.Name)
		}

		return cmp
	})

	listSnapEntries := make([]*csi.ListSnapshotsResponse_Entry, 0, len(snapshotList.Items)-startingToken)
	nextToken := ""

	// if SourceVolumeId is specified, return matching volumes
	// otherwise return volumes from global list
	counter := 0
	for i := startingToken; i < len(snapshotList.Items); i++ {
		entry := getListSnapshotEntry(&snapshotList.Items[i], req.SourceVolumeId)
		if entry != nil {
			if counter == maxEntries {
				nextToken = strconv.Itoa(counter)
				break
			}

			listSnapEntries = append(listSnapEntries, entry)
			counter++
		}
	}

	// if startingToken is not at the beginning of the list, that implies that there exists a match at startingToken
	if len(listSnapEntries) == 0 && startingToken != 0 {
		return nil, status.Error(codes.Aborted, "StartingToken no longer valid")
	}

	return &csi.ListSnapshotsResponse{
		Entries:   listSnapEntries,
		NextToken: nextToken,
	}, nil
}

// Helper function for ListSnapshots() that returns a populated ListSnapshotsResponse_Entry.
// Filters entries based on whether a sourceVolume was provided
func getListSnapshotEntry(snapshot *v1alpha1.Snapshot, sourceVolume string) *csi.ListSnapshotsResponse_Entry {
	cond := meta.FindStatusCondition(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable)
	if cond == nil {
		return nil
	}

	if sourceVolume != "" && sourceVolume != snapshot.Spec.SourceVolume {
		return nil
	}

	return &csi.ListSnapshotsResponse_Entry{
		Snapshot: &csi.Snapshot{
			SizeBytes:      snapshot.Status.SizeBytes,
			SnapshotId:     snapshot.Name,
			SourceVolumeId: snapshot.Spec.SourceVolume,
			CreationTime:   timestamppb.New(cond.LastTransitionTime.Time),
			ReadyToUse:     true,
		},
	}
}
