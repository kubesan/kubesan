// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/csi/common/validate"
)

func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	// validate request

	namespacedName, err := validate.ValidateVolumeID(req.SourceVolumeId)
	if err != nil {
		return nil, err
	}
	volume := &v1alpha1.Volume{}
	err = s.client.Get(ctx, namespacedName, volume)
	if errors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	if err != nil {
		return nil, err
	}

	switch {
	case volume.Spec.Mode != v1alpha1.VolumeModeThin:
		return nil, status.Error(codes.Unimplemented, "snapshots are only supported on thin volumes")

	case volume.Spec.Type.Filesystem != nil:
		return nil, status.Error(codes.Unimplemented, "filesystem snapshots not implemented yet")

	case req.Name == "":
		return nil, status.Error(codes.InvalidArgument, "must specify snapshot name")

	// At present, we don't use secrets, so this should be empty.
	case len(req.Secrets) > 0:
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")

	// At present, we only expect parameters synthesized by
	// --extra-create-metadata in controller-plugin.yaml, to get at
	// the VS/VSC name and namespace.
	case len(req.Parameters) > 0:
		for key := range req.Parameters {
			switch key {
			default:
				return nil, status.Error(codes.InvalidArgument, "unexpected parameters")
			case "csi.storage.k8s.io/volumesnapshot/name":
			case "csi.storage.k8s.io/volumesnapshot/namespace":
			case "csi.storage.k8s.io/volumesnapshotcontent/name":
			}
		}
	}

	name := validate.SafeName(req.Name, "snapshot")
	binding := req.Name
	if binding == name {
		vsNamespace := req.Parameters["csi.storage.k8s.io/volumesnapshot/namespace"]
		vsName := req.Parameters["csi.storage.k8s.io/volumesnapshot/name"]
		binding = vsNamespace + "/" + vsName
	}

	// create snapshot

	snapshot := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
			Labels: map[string]string{
				config.AppNameLabel:    "kubesan",
				config.AppVersionLabel: config.Version,
			},
		},
		Spec: v1alpha1.SnapshotSpec{
			VgName:       volume.Spec.VgName,
			Binding:      binding,
			SourceVolume: req.SourceVolumeId,
		},
	}
	controllerutil.AddFinalizer(snapshot, config.Finalizer)

	err = s.client.Create(ctx, snapshot)
	if errors.IsAlreadyExists(err) {
		// Check that the new request is idempotent to the existing snapshot
		err = s.client.Get(ctx, types.NamespacedName{Name: snapshot.Name, Namespace: config.Namespace}, snapshot)
		if err == nil && req.SourceVolumeId != snapshot.Spec.SourceVolume {
			err = status.Error(codes.AlreadyExists, "snapshot already exists with different source")

		}
	}
	if err != nil {
		return nil, err
	}

	err = s.client.WatchSnapshotUntil(ctx, snapshot, func() bool {
		return meta.IsStatusConditionTrue(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable)
	})
	if err != nil {
		return nil, err
	}

	condition := meta.FindStatusCondition(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable)

	resp := &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      snapshot.Status.SizeBytes,
			SnapshotId:     snapshot.Name,
			SourceVolumeId: snapshot.Spec.SourceVolume,
			CreationTime:   timestamppb.New(condition.LastTransitionTime.Time),
			ReadyToUse:     true,
		},
	}

	return resp, nil
}

func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	// validate request

	if _, err := validate.ValidateSnapshotID(req.SnapshotId); err != nil {
		return nil, err
	}

	// At present, we don't use secrets, so this should be empty.
	if len(req.Secrets) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected secrets")
	}

	// delete snapshot

	snapshot := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.SnapshotId,
			Namespace: config.Namespace,
		},
	}

	if err := s.client.DeleteAndConfirm(ctx, snapshot); err != nil {
		return nil, err
	}

	// success

	resp := &csi.DeleteSnapshotResponse{}

	return resp, nil
}

// Get a (possibly filtered) list of snapshots.
func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {

	// retrieve all snapshots
	// TODO consider using client.MatchingFields for faster listing when req.SourceVolumeId is set
	snapshotList := &v1alpha1.SnapshotList{}
	if err := s.client.List(ctx, snapshotList); err != nil {
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
