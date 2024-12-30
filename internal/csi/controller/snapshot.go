// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/csi/common/validate"
)

func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	// validate request

	if _, err := validate.ValidateVolumeID(req.SourceVolumeId); err != nil {
		return nil, err
	}

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "must specify snapshot name")
	}

	// create snapshot

	snapshot := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: config.Namespace,
		},
		Spec: v1alpha1.SnapshotSpec{
			SourceVolume: req.SourceVolumeId,
		},
	}
	controllerutil.AddFinalizer(snapshot, config.Finalizer)

	if err := s.client.Create(ctx, snapshot); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	err := s.client.WatchSnapshotUntil(ctx, snapshot, func() bool {
		return meta.IsStatusConditionTrue(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable)
	})
	if err != nil {
		return nil, err
	}

	condition := meta.FindStatusCondition(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable)

	resp := &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      *snapshot.Status.SizeBytes,
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

	// success

	resp := &csi.DeleteSnapshotResponse{}

	return resp, nil
}
