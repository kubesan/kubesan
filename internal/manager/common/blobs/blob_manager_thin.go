// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"
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
	client client.Client
	scheme *runtime.Scheme
	vgName string
}

// NewThinBlobManager returns a BlobManager implemented using LVM's thin
// logical volumes. Thin LVs are thinly provisioned and support snapshots.
// Direct ReadWriteMany access is not supported and needs to be provided via
// another means like NBD. Thin LVs are good for general use cases and virtual
// machines.
func NewThinBlobManager(client client.Client, scheme *runtime.Scheme, vgName string) BlobManager {
	return &ThinBlobManager{
		client: client,
		scheme: scheme,
		vgName: vgName,
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
	// Give the pool 1% more space than the volume, to account for any metadata overhead
	// TODO: this is wasteful for large sparse volumes once auto-extend is working. Find a better heuristic for this, maybe max(min(size, 1G),size/10)
	paddedSize := sizeBytes + ((sizeBytes/100)+511)/512*512
	thinPoolLv := &v1alpha1.ThinPoolLv{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
		},
		Spec: v1alpha1.ThinPoolLvSpec{
			VgName:    m.vgName,
			SizeBytes: paddedSize,
		},
	}
	controllerutil.AddFinalizer(thinPoolLv, config.Finalizer)

	if err := controllerutil.SetOwnerReference(owner, thinPoolLv, m.scheme); err != nil {
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
func (m *ThinBlobManager) createThinLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv, name string, sizeBytes int64) error {
	thinlv := &v1alpha1.ThinLvSpec{
		Name: name,
		Contents: v1alpha1.ThinLvContents{
			ContentsType: v1alpha1.ThinLvContentsTypeEmpty,
		},
		ReadOnly:  false, // TODO fill in?
		SizeBytes: sizeBytes,
		State: v1alpha1.ThinLvSpecState{
			Name: v1alpha1.ThinLvSpecStateNameInactive,
		},
	}

	// update resource if Spec.ThinLvs[] changed

	old := thinPoolLv.Spec.FindThinLv(name)
	if old == nil {
		thinPoolLv.Spec.ThinLvs = append(thinPoolLv.Spec.ThinLvs, *thinlv)
	} else if *old == *thinlv {
		return nil // no change
	} else {
		*old = *thinlv
	}

	return thinpoollv.UpdateThinPoolLv(ctx, m.client, nil, thinPoolLv)
}

// Is the thin LV listed in Status.ThinLvs[] with the correct size?
func (m *ThinBlobManager) checkThinLvExists(thinPoolLv *v1alpha1.ThinPoolLv, name string, sizeBytes int64) bool {
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)
	return thinLvStatus != nil && thinLvStatus.SizeBytes == sizeBytes
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

func (m *ThinBlobManager) CreateBlob(ctx context.Context, name string, sizeBytes int64, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.createThinPoolLv(ctx, name, sizeBytes, owner)
	if err != nil {
		log.Error(err, "CreateBlob createThinPoolLv failed")
		return err
	}
	oldThinPoolLv := thinPoolLv.DeepCopy()

	thinLvName := thinpoollv.VolumeToThinLvName(name)
	err = m.createThinLv(ctx, thinPoolLv, thinLvName, sizeBytes)
	if err != nil {
		log.Error(err, "CreateBlob createThinLv failed")
		return err
	}

	if !m.checkThinLvExists(thinPoolLv, thinLvName, sizeBytes) {
		return &util.WatchPending{}
	}
	// TODO propagate back errors

	// update thinPoolLv to clear Spec.ActiveOnNode, if necessary

	err = thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
	if err != nil {
		log.Error(err, "CreateBlob UpdateThinPoolLv failed")
		return err
	}

	// TODO recreate if size does not match. This handles the case where a
	// blob was partially created and then reconciled again with a
	// different size. A blob must never be recreated after volume creation
	// has completed since that could lose data!
	return err
}

func (m *ThinBlobManager) RemoveBlob(ctx context.Context, name string, owner client.Object) error {
	log := log.FromContext(ctx).WithValues("blobName", name, "nodeName", config.LocalNodeName)

	thinPoolLv, err := m.getThinPoolLv(ctx, name)
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
		return &util.WatchPending{}
	}

	err = m.forgetRemovedThinLv(ctx, thinPoolLv, thinLvName)
	if err != nil {
		log.Error(err, "RemoveBlob forgetRemovedThinLv failed")
		return err
	}
	if thinPoolLv.Status.FindThinLv(thinLvName) != nil {
		return &util.WatchPending{}
	}

	// update thinPoolLv to clear Spec.ActiveOnNode, if necessary

	err = thinpoollv.UpdateThinPoolLv(ctx, m.client, oldThinPoolLv, thinPoolLv)
	if err != nil {
		log.Error(err, "RemoveBlob UpdateThinPoolLv failed")
		return err
	}

	return nil
}

func (m *ThinBlobManager) GetPath(name string) string {
	return dm.GetDevicePath(name)
}
