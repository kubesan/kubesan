// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"
	"fmt"
	"slices"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/common/dm"
	"gitlab.com/kubesan/kubesan/internal/manager/common/thinpoollv"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ThinBlobManager struct {
	client   client.Client
	scheme   *runtime.Scheme
	vgName   string
	poolName string
}

// NewThinBlobManager returns a BlobManager implemented using LVM's thin
// logical volumes. Thin LVs are thinly provisioned and support snapshots.
// Direct ReadWriteMany access is not supported and needs to be provided via
// another means like NBD. Thin LVs are good for general use cases and virtual
// machines.
//
// vgName may be empty if the manager will only be used for SnapshotBlob
// rather than CreateBlob (since the source of a snapshot already determines
// the VG that the thin pool lives in).
func NewThinBlobManager(client client.Client, scheme *runtime.Scheme, vgName string, poolName string) BlobManager {
	return &ThinBlobManager{
		client:   client,
		scheme:   scheme,
		vgName:   vgName,
		poolName: poolName,
	}
}

func (m *ThinBlobManager) getThinPoolLv(ctx context.Context, name string) (*v1alpha1.ThinPoolLv, error) {
	thinPoolLv := &v1alpha1.ThinPoolLv{}

	if err := m.client.Get(ctx, types.NamespacedName{Name: name, Namespace: config.Namespace}, thinPoolLv); err != nil {
		return nil, err
	}

	return thinPoolLv, nil
}

func (m *ThinBlobManager) createThinPoolLv(ctx context.Context, name string, sizeBytes int64, owner client.Object) (*v1alpha1.ThinPoolLv, error) {
	thinPoolLv := &v1alpha1.ThinPoolLv{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
			Labels:    config.CommonLabels,
		},
		Spec: v1alpha1.ThinPoolLvSpec{
			VgName: m.vgName,
			// Let the thin-pool grow after the first gigabyte
			SizeBytes: min(sizeBytes, 1024*1024*1024),
		},
	}
	controllerutil.AddFinalizer(thinPoolLv, config.Finalizer)

	if err := controllerutil.SetOwnerReference(owner, thinPoolLv, m.scheme, controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return nil, err
	}

	if err := m.client.Create(ctx, thinPoolLv); err != nil {
		if errors.IsAlreadyExists(err) {
			return m.getThinPoolLv(ctx, name)
		}
		return nil, err
	}
	return thinPoolLv, nil
}

// Add or update ThinLvSpec in ThinPoolLv.Spec.ThinLvs[]
func (m *ThinBlobManager) createThinLv(ctx context.Context, oldThinPoolLv, thinPoolLv *v1alpha1.ThinPoolLv, name string, binding string, sizeBytes int64, contents *v1alpha1.ThinLvContents) error {
	readOnly := contents.ContentsType == v1alpha1.ThinLvContentsTypeSnapshot
	thinLvSpec := thinPoolLv.Spec.FindThinLv(name)
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)

	if thinLvSpec == nil {
		thinLvSpec = &v1alpha1.ThinLvSpec{
			Name:      name,
			Binding:   binding,
			Contents:  *contents,
			ReadOnly:  readOnly,
			SizeBytes: sizeBytes,
			State: v1alpha1.ThinLvSpecState{
				Name: v1alpha1.ThinLvSpecStateNameInactive,
			},
		}
		thinPoolLv.Spec.ThinLvs = append(thinPoolLv.Spec.ThinLvs, *thinLvSpec)
	} else if thinLvStatus == nil || sizeBytes > thinLvStatus.SizeBytes {
		thinLvSpec.SizeBytes = sizeBytes
	}

	return thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
}

// See if the thin LV is listed in Status.ThinLvs[] with sufficient size.
func (m *ThinBlobManager) checkThinLvExists(thinPoolLv *v1alpha1.ThinPoolLv, name string, sizeBytes int64) bool {
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)
	return thinLvStatus != nil && thinLvStatus.SizeBytes >= sizeBytes
}

// Is the thin LV absent from Status.ThinLvs[] or marked as removed?
func (m *ThinBlobManager) checkThinLvRemoved(thinPoolLv *v1alpha1.ThinPoolLv, name string) bool {
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)
	return thinLvStatus == nil || thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameRemoved
}

func (m *ThinBlobManager) requestThinLvRemoval(ctx context.Context, oldThinPoolLv, thinPoolLv *v1alpha1.ThinPoolLv, name string) error {
	thinLvSpec := thinPoolLv.Spec.FindThinLv(name)
	if thinLvSpec == nil {
		return nil // treated as already removed
	}

	if thinLvSpec.State.Name != v1alpha1.ThinLvSpecStateNameRemoved {
		thinLvSpec.State = v1alpha1.ThinLvSpecState{
			Name: v1alpha1.ThinLvSpecStateNameRemoved,
		}
	}

	return thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
}

func (m *ThinBlobManager) forgetRemovedThinLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv, name string) error {
	for i := range thinPoolLv.Spec.ThinLvs {
		if thinPoolLv.Spec.ThinLvs[i].Name == name {
			thinPoolLv.Spec.ThinLvs = slices.Delete(thinPoolLv.Spec.ThinLvs, i, i+1)
			return thinpoollv.UpdateThinPoolLv(ctx, m.client, nil, thinPoolLv)
		}
	}
	return nil // not found, treat as already deleted
}

// Create or expand a thin volume blob within the manager's pool.
func (m *ThinBlobManager) CreateBlob(ctx context.Context, name string, binding string, sizeBytes int64, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.createThinPoolLv(ctx, name, sizeBytes, owner)
	if err != nil {
		log.Error(err, "CreateBlob createThinPoolLv failed")
		return err
	}
	oldThinPoolLv := thinPoolLv.DeepCopy()

	thinLvName := thinpoollv.VolumeToThinLvName(name)
	contents := &v1alpha1.ThinLvContents{ContentsType: v1alpha1.ThinLvContentsTypeEmpty}
	err = m.createThinLv(ctx, oldThinPoolLv, thinPoolLv, thinLvName, binding, sizeBytes, contents)
	if err != nil {
		log.Error(err, "CreateBlob createThinLv failed")
		return err
	}

	if !m.checkThinLvExists(thinPoolLv, thinLvName, sizeBytes) {
		return util.NewWatchPending("waiting for thin pool creation")
	}
	// TODO propagate back errors

	// update thinPoolLv to clear Spec.ActiveOnNode, if necessary

	err = thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
	if err != nil {
		log.Error(err, "CreateBlob UpdateThinPoolLv failed")
		return err
	}

	return err
}

// This returns the offline size. A staged volume may still see a smaller
// size until its device-mapper wrapper is resized.
func (m *ThinBlobManager) GetSize(ctx context.Context, name string) (int64, error) {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		return 0, err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(name)
	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	if thinLvStatus == nil {
		return 0, errors.NewBadRequest(fmt.Sprintf("thinLv \"%s\" not found", thinLvName))
	}

	log.Info("GetSize complete", "size", thinLvStatus.SizeBytes)
	return thinLvStatus.SizeBytes, nil
}

func (m *ThinBlobManager) SnapshotBlob(ctx context.Context, name string, binding string, sourceName string, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	log.Info("SnapshotBlob entered", "name", name)
	defer log.Info("SnapshotBlob exited")

	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		log.Error(err, "SnapshotBlob getThinPoolLv failed")
		return err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()

	sourceThinLv := thinPoolLv.Spec.FindThinLv(thinpoollv.VolumeToThinLvName(sourceName))
	if sourceThinLv == nil || sourceThinLv.State.Name == v1alpha1.ThinLvSpecStateNameRemoved {
		log.Error(err, "SnapshotBlob sourceThinLv not found")
		return errors.NewBadRequest("sourceThinLv not found")
	}

	if err = controllerutil.SetOwnerReference(owner, thinPoolLv, m.scheme, controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(name)
	contents := &v1alpha1.ThinLvContents{
		ContentsType: v1alpha1.ThinLvContentsTypeSnapshot,
		Snapshot: &v1alpha1.ThinLvContentsSnapshot{
			SourceThinLvName: sourceThinLv.Name,
		}}
	// Snapshots are sized at the time of lvcreate, so we don't need
	// to specify a size in the spec.
	err = m.createThinLv(ctx, oldThinPoolLv, thinPoolLv, thinLvName, binding, 0, contents)
	if err != nil {
		log.Error(err, "SnapshotBlob createThinLv failed")
		return err
	}

	if !m.checkThinLvExists(thinPoolLv, thinLvName, 0) {
		return util.NewWatchPending("waiting for blob creation")
	}
	// TODO propagate back errors

	// update thinPoolLv to clear Spec.ActiveOnNode, if necessary

	err = thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
	if err != nil {
		log.Error(err, "CreateBlob UpdateThinPoolLv failed")
		return err
	}
	return nil
}

func (m *ThinBlobManager) RemoveBlob(ctx context.Context, name string, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "thinPoolLvName", m.poolName, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "RemoveBlob getThinPoolLv failed")
		return err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()
	thinLvName := thinpoollv.VolumeToThinLvName(name)
	err = m.requestThinLvRemoval(ctx, oldThinPoolLv, thinPoolLv, thinLvName)
	if err != nil {
		log.Error(err, "RemoveBlob requestThinLvRemoval failed")
		return err
	}

	if !m.checkThinLvRemoved(thinPoolLv, thinLvName) {
		return util.NewWatchPending("waiting for blob removal")
	}

	err = m.forgetRemovedThinLv(ctx, thinPoolLv, thinLvName)
	if err != nil {
		log.Error(err, "RemoveBlob forgetRemovedThinLv failed")
		return err
	}
	if thinPoolLv.Status.FindThinLv(thinLvName) != nil {
		return util.NewWatchPending("waiting for thinlv cleanup")
	}

	// Normally when a Volume or Snapshot is deleted k8s automatically
	// removes the owner reference; and if it was the last owner reference,
	// it also marks the ThinPoolLv for deletion.  But cloning creates
	// temporary snapshot LVs that are removed when cloning completes,
	// while the target Volume still lives for a long time. If this was a
	// clone reference, remove the owner reference here, but ignore errors
	// in case k8s already removed it.  And if removing the clone owner
	// reference leaves no ThinLVs, mark the ThinPoolLv for deletion.
	_ = controllerutil.RemoveOwnerReference(owner, thinPoolLv, m.scheme)
	if len(thinPoolLv.Spec.ThinLvs) == 0 && thinPoolLv.DeletionTimestamp == nil {
		if err := m.client.Delete(ctx, thinPoolLv); err != nil {
			return client.IgnoreNotFound(err)
		}
	}

	// update thinPoolLv to clear Spec.ActiveOnNode, if necessary

	err = thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
	if err != nil {
		log.Error(err, "RemoveBlob UpdateThinPoolLv failed")
		return err
	}

	return nil
}

func (m *ThinBlobManager) ActivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		return err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()

	// add owner reference so controller reconcile is called when ActivateThinLv() watches ThinPoolLv
	if err := controllerutil.SetOwnerReference(owner, thinPoolLv, m.scheme, controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return err
	}

	return thinpoollv.ActivateThinLv(ctx, m.client, oldThinPoolLv, thinPoolLv, name)
}

func (m *ThinBlobManager) ActivateBlobForCloneTarget(ctx context.Context, name string, dataSrcBlobMgr BlobManager) (string, error) {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	if dataSrcBlobMgr == nil {
		// nothing to clone for empty source
		return "", nil
	}

	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		return "", err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()

	// Activate the target blob on the same node as the source blob. This
	// assumes the source blob was activated using
	// ActivateBlobForCloneSource.

	dataSrcThinBlobMgr, ok := dataSrcBlobMgr.(*ThinBlobManager)
	if !ok {
		log.Info("Data source is not a thin-pool")
		return "", errors.NewBadRequest("Data source is not a thin-pool!")
	}

	dataSrcThinPoolLv, err := m.getThinPoolLv(ctx, dataSrcThinBlobMgr.poolName)
	if err != nil {
		return "", err
	}

	if thinPoolLv.Spec.ActiveOnNode == dataSrcThinPoolLv.Status.ActiveOnNode {
		log.Info("Clone target ThinPoolLv already active on data source node")
	} else {
		log.Info("Setting clone target ThinPoolLv to become active on data source node")
	}
	thinPoolLv.Spec.ActiveOnNode = dataSrcThinPoolLv.Status.ActiveOnNode

	return thinPoolLv.Spec.ActiveOnNode, thinpoollv.ActivateThinLv(ctx, m.client, oldThinPoolLv, thinPoolLv, name)
}

func (m *ThinBlobManager) DeactivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // already deleted
		}
		return err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()

	log.Info("Clone source no longer in use as a data source, deactivating")

	if thinPoolLv.Spec.FindThinLv(thinpoollv.VolumeToThinLvName(name)) == nil {
		return nil // already removed
	}

	return thinpoollv.DeactivateThinLv(ctx, m.client, oldThinPoolLv, thinPoolLv, name)
}

func (m *ThinBlobManager) DeactivateBlobForCloneTarget(ctx context.Context, name string) error {
	thinPoolLv, err := m.getThinPoolLv(ctx, m.poolName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // already deleted
		}
		return err
	}

	oldThinPoolLv := thinPoolLv.DeepCopy()

	if thinPoolLv.Spec.FindThinLv(thinpoollv.VolumeToThinLvName(name)) == nil {
		return nil // already removed
	}

	return thinpoollv.DeactivateThinLv(ctx, m.client, oldThinPoolLv, thinPoolLv, name)
}

func (m *ThinBlobManager) GetPath(name string) string {
	return dm.GetDevicePath(name)
}

// Thin blobs can be expanded online.
func (m *ThinBlobManager) ExpansionMustBeOffline() bool {
	return false
}

// If the blob is staged, the active node controller will update size.  The
// cluster controller only needs to step in when the blob is offline.
func (m *ThinBlobManager) SizeNeedsCheck(staged bool) bool {
	return !staged
}
