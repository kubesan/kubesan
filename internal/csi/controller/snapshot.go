// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"log"
	"math"
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

	// At present, we don't support any parameters, so this should
	// be empty.  Note that passing --extra-create-metadata in
	// controller-plugin.yaml would change this situation, but our
	// current design does not need to get the VolumeSnapshot object.
	// See https://gitlab.com/kubesan/kubesan/-/issues/105.
	case len(req.Parameters) > 0:
		return nil, status.Error(codes.InvalidArgument, "unexpected parameters")
	}

	// create snapshot

	snapshot := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      validate.SafeName(req.Name, "snapshot"),
			Namespace: config.Namespace,
		},
		Spec: v1alpha1.SnapshotSpec{
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

	// delete snapshot

	snapshot := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.SnapshotId,
			Namespace: config.Namespace,
		},
	}

	propagation := client.PropagationPolicy(metav1.DeletePropagationForeground)

	if err := s.client.Delete(ctx, snapshot, propagation); err != nil && !errors.IsNotFound(err) {
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
		err := s.client.Get(ctx, types.NamespacedName{Name: req.SnapshotId, Namespace: config.Namespace}, snapshot)
		if err == nil {
			log.Printf("Snapshot \"%v\" still exists", req.SnapshotId)
			return false, nil // keep going
		} else if errors.IsNotFound(err) {
			log.Printf("Snapshot \"%v\" deleted", req.SnapshotId)
			return true, nil // done
		} else {
			log.Printf("Snapshot \"%v\" Get() failed: %+v", req.SnapshotId, err)
			return false, err
		}
	})
	if err != nil {
		return nil, err
	}

	// success

	resp := &csi.DeleteSnapshotResponse{}

	return resp, nil
}
