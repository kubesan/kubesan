// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
	"gitlab.com/kubesan/kubesan/internal/manager/common/blobs"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
)

type VolumeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpVolumeReconciler(mgr ctrl.Manager) error {
	r := &VolumeReconciler{
		Client: client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		For(&v1alpha1.Volume{}).
		Owns(&v1alpha1.ThinPoolLv{}, builder.MatchEveryOwner). // for ThinBlobManager
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/finalizers,verbs=update,namespace=kubesan-system

func (r *VolumeReconciler) newBlobManager(volume *v1alpha1.Volume) (blobs.BlobManager, error) {
	switch volume.Spec.Mode {
	case v1alpha1.VolumeModeThin:
		return blobs.NewThinBlobManager(r.Client, r.Scheme, volume.Spec.VgName, volume.Name), nil
	case v1alpha1.VolumeModeLinear:
		return blobs.NewLinearBlobManager(volume.Spec.VgName), nil
	default:
		return nil, errors.NewBadRequest("invalid volume mode")
	}
}

// Returns the name of a temporary snapshot for volume cloning
func cloneVolumeSnapshotName(volumeName string) string {
	return "clone-" + volumeName
}

// Get a BlobManager, blob name, and pool for a given volume's data source.
func (r *VolumeReconciler) getDataSourceBlob(ctx context.Context, volume *v1alpha1.Volume) (blobs.BlobManager, string, string, error) {
	switch volume.Spec.Contents.ContentsType {
	case v1alpha1.VolumeContentsTypeCloneVolume:
		name := volume.Spec.Contents.CloneVolume.SourceVolume

		dataSrcVolume := &v1alpha1.Volume{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: config.Namespace}, dataSrcVolume); err != nil {
			return nil, "", "", err
		}

		blobMgr, err := r.newBlobManager(dataSrcVolume)
		if err != nil {
			return nil, "", "", err
		}
		return blobMgr, name, name, nil
	case v1alpha1.VolumeContentsTypeCloneSnapshot:
		name := volume.Spec.Contents.CloneSnapshot.SourceSnapshot

		dataSrcSnapshot := &v1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: config.Namespace}, dataSrcSnapshot); err != nil {
			return nil, "", "", err
		}

		blobMgr := blobs.NewThinBlobManager(r.Client, r.Scheme, dataSrcSnapshot.Spec.VgName, dataSrcSnapshot.Spec.SourceVolume)
		return blobMgr, name, dataSrcSnapshot.Spec.SourceVolume, nil
	default:
		return nil, "", "", nil
	}
}

func (r *VolumeReconciler) activateDataSource(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	dataSrcBlobMgr, dataSrcBlobName, dataSrcPool, err := r.getDataSourceBlob(ctx, volume)
	if err != nil {
		return err
	}

	if dataSrcBlobMgr != nil {
		// create a temporary snapshot to clone from

		tmpBlobName := cloneVolumeSnapshotName(volume.Name)
		if err := dataSrcBlobMgr.SnapshotBlob(ctx, tmpBlobName, "", dataSrcBlobName, volume); err != nil {
			return err
		}

		log.Info("Temporary snapshot created for cloning", "tmpBlobName", tmpBlobName)

		if err := dataSrcBlobMgr.ActivateBlobForCloneSource(ctx, tmpBlobName, volume); err != nil {
			return err
		}

		log.Info("Activated source blob for cloning", "tmpBlobName", tmpBlobName)
	}

	node, err := blobMgr.ActivateBlobForCloneTarget(ctx, volume.Name, dataSrcBlobMgr)
	if err != nil || node == "" {
		return err
	}

	log.Info("Activated target blob for cloning", "volumeName", volume.Name)

	// We need to get back to the same dataSrcBlobMgr, even if the
	// source Volume or Snapshot is deleted in the meantime. Do so
	// by storing in a label of the destination Volume.
	labels := volume.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, exists := labels[config.PopulationNodeLabel]; !exists {
		labels[config.PopulationNodeLabel] = node
		if dataSrcPool != "" {
			labels[config.CloneSourceLabel] = dataSrcPool
		}
		volume.SetLabels(labels)
		if err := r.Update(ctx, volume); err != nil {
			return err
		}
		log.Info("Added labels to volume", "volume", volume.Name, "pool", dataSrcPool, "populationNode", node)
	}

	return nil
}

func (r *VolumeReconciler) deactivateDataSource(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if err := blobMgr.DeactivateBlobForCloneTarget(ctx, volume.Name); err != nil {
		return err
	}

	log.Info("Deactivated target blob for cloning", "volumeName", volume.Name)

	labels := volume.GetLabels()
	if labels == nil {
		// Assume we already finished cleanup.
		return nil
	}
	if pool, exists := labels[config.CloneSourceLabel]; exists {
		tmpBlobName := cloneVolumeSnapshotName(volume.Name)
		dataSrcBlobMgr := blobs.NewThinBlobManager(r.Client, r.Scheme, volume.Spec.VgName, pool)

		log.Info("About to deactivate source blob for cloning", "tmpBlobName", tmpBlobName)

		if err := dataSrcBlobMgr.DeactivateBlobForCloneSource(ctx, tmpBlobName, volume); err != nil {
			return err
		}

		log.Info("Deactivated source blob for cloning", "tmpBlobName", tmpBlobName)

		// delete temporary snapshot

		if err := dataSrcBlobMgr.RemoveBlob(ctx, tmpBlobName, volume); err != nil {
			return err
		}
		log.Info("Removed temporary snapshot for cloning", "tmpBlobName", tmpBlobName)
	}

	// clean up labels
	if _, exists := labels[config.PopulationNodeLabel]; exists {
		delete(labels, config.PopulationNodeLabel)
		delete(labels, config.CloneSourceLabel)
		volume.SetLabels(labels)
		if err := r.Update(ctx, volume); err != nil {
			return err
		}
		log.Info("Deleted label from volume", "volume", volume.Name)
	}

	return nil
}

func (r *VolumeReconciler) reconcileDeleting(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if len(volume.Status.AttachedToNodes) > 0 {
		log.Info("reconcileDeleting waiting for AttachedToNodes[] to become empty")
		return nil // wait until no longer attached
	}

	if err := r.deactivateDataSource(ctx, blobMgr, volume); err != nil {
		return err
	}

	if err := blobMgr.RemoveBlob(ctx, volume.Name, volume); err != nil {
		// During deletion, if there were multiple
		// OwnerReferences, we have no guarantee when
		// kubernetes will remove our reference.  But once our
		// reference is gone, we no longer get reconcile
		// events when the child changes state, so we must let
		// WatchPending errors through to trigger exponential
		// backoff so we can still poll the ThinPoolLv.
		return err
	}

	log.Info("RemoveBlob succeeded")

	if controllerutil.RemoveFinalizer(volume, config.Finalizer) {
		if err := r.Update(ctx, volume); err != nil {
			return err
		}
	}
	return nil
}

// Prepare a Volume for expansion.
func (r *VolumeReconciler) reconcilePrepareForResize(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume, online bool) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)
	needsUpdate := false

	if volume.Spec.Type.Filesystem != nil {
		condition := metav1.Condition{
			Type:    v1alpha1.VolumeConditionAbnormal,
			Status:  metav1.ConditionTrue,
			Reason:  "ResizeUnsupported",
			Message: "filesystem resize not implemented yet",
		}
		if meta.SetStatusCondition(&volume.Status.Conditions, condition) {
			needsUpdate = true
		}
	} else {
		log.Info("resize needed")
		if blobMgr.ExpansionMustBeOffline() {
			if online {
				return util.NewWatchPending("waiting for volume to be offline")
			}
			condition1 := metav1.Condition{
				Type:    v1alpha1.VolumeConditionDataSourceCompleted,
				Status:  metav1.ConditionFalse,
				Reason:  "Resizing",
				Message: "volume is unavailable while resizing",
			}
			condition2 := metav1.Condition{
				Type:    v1alpha1.VolumeConditionAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Resizing",
				Message: "volume is unavailable while resizing",
			}
			condition3 := metav1.Condition{
				Type:    v1alpha1.VolumeConditionAbnormal,
				Status:  metav1.ConditionTrue,
				Reason:  "Resizing",
				Message: "volume unavailable during expansion",
			}
			if meta.SetStatusCondition(&volume.Status.Conditions, condition1) ||
				meta.SetStatusCondition(&volume.Status.Conditions, condition2) ||
				meta.SetStatusCondition(&volume.Status.Conditions, condition3) {
				needsUpdate = true
			}
		}
	}
	if needsUpdate {
		return r.statusUpdate(ctx, volume)
	}
	return nil
}

// Reconcile cluster-wide aspects of a Volume that has not been deleted.
func (r *VolumeReconciler) reconcileNotDeleting(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)
	needsUpdate := false
	needsResize := volume.Status.SizeBytes > 0 && volume.Spec.SizeBytes > volume.Status.SizeBytes
	online := len(volume.Spec.AttachToNodes)+len(volume.Status.AttachedToNodes) > 0

	// add finalizer

	if controllerutil.AddFinalizer(volume, config.Finalizer) {
		if err := r.Update(ctx, volume); err != nil {
			return err
		}
	}

	// Prepare for a resize, if needed.

	if needsResize {
		if err := r.reconcilePrepareForResize(ctx, blobMgr, volume, online); err != nil {
			return err
		}
	}

	// create or expand LVM LV if necessary

	if !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionLvCreated) || needsResize {
		if err := blobMgr.CreateBlob(ctx, volume.Name, volume.Spec.Binding, volume.Spec.SizeBytes, volume); err != nil {
			return err
		}

		log.Info("CreateBlob succeeded")

		volume.Status.Path = blobMgr.GetPath(volume.Name)
		if blobMgr.SizeNeedsCheck(online) {
			sizeBytes, err := blobMgr.GetSize(ctx, volume.Name)
			if err != nil {
				return err
			}
			if sizeBytes > volume.Status.SizeBytes {
				needsUpdate = true
				volume.Status.SizeBytes = sizeBytes
			}
		}
		condition := metav1.Condition{
			Type:    v1alpha1.VolumeConditionLvCreated,
			Status:  metav1.ConditionTrue,
			Reason:  "Created",
			Message: "lvm logical volume created",
		}
		if meta.SetStatusCondition(&volume.Status.Conditions, condition) {
			needsUpdate = true
		}

		if needsUpdate {
			if err := r.statusUpdate(ctx, volume); err != nil {
				return err
			}
		}
		needsUpdate = false
	}

	// Ensure initial contents are populated.

	if !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionDataSourceCompleted) {
		if err := r.activateDataSource(ctx, blobMgr, volume); err != nil {
			return err
		}

		log.Info("activateDataSource succeeded")

		// Shortcut when data population is not required
		if volume.Spec.Contents.ContentsType == v1alpha1.VolumeContentsTypeEmpty && !blobMgr.ExpansionMustBeOffline() {
			condition := metav1.Condition{
				Type:    v1alpha1.VolumeConditionDataSourceCompleted,
				Status:  metav1.ConditionTrue,
				Reason:  "Completed",
				Message: "population from data source completed",
			}
			if meta.SetStatusCondition(&volume.Status.Conditions, condition) {
				needsUpdate = true
			}
		}
	}

	if !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionAvailable) &&
		meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionDataSourceCompleted) {
		if err := r.deactivateDataSource(ctx, blobMgr, volume); err != nil {
			return err
		}

		condition := metav1.Condition{
			Type:    v1alpha1.VolumeConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Available",
			Message: "volume ready for use",
		}
		meta.SetStatusCondition(&volume.Status.Conditions, condition)
		condition = metav1.Condition{
			Type:    v1alpha1.VolumeConditionAbnormal,
			Status:  metav1.ConditionFalse,
			Reason:  "Available",
			Message: "volume ready for use",
		}
		meta.SetStatusCondition(&volume.Status.Conditions, condition)

		needsUpdate = true
	}

	if needsUpdate {
		if err := r.statusUpdate(ctx, volume); err != nil {
			return err
		}
	}

	return nil
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("VolumeReconciler entered")
	defer log.Info("VolumeReconciler exited")

	volume := &v1alpha1.Volume{}
	if err := r.Get(ctx, req.NamespacedName, volume); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	blobMgr, err := r.newBlobManager(volume)
	if err != nil {
		return ctrl.Result{}, err
	}

	if volume.DeletionTimestamp != nil {
		err := r.reconcileDeleting(ctx, blobMgr, volume)
		return ctrl.Result{}, err
	}

	if kubesanslices.CountNonNil(
		volume.Spec.Type.Block,
		volume.Spec.Type.Filesystem,
	) != 1 {
		return ctrl.Result{}, errors.NewBadRequest("invalid volume type")
	}

	switch volume.Spec.Contents.ContentsType {
	case v1alpha1.VolumeContentsTypeEmpty:
		if kubesanslices.CountNonNil(
			volume.Spec.Contents.CloneVolume,
			volume.Spec.Contents.CloneSnapshot,
		) != 0 {
			return ctrl.Result{}, errors.NewBadRequest("invalid volume contents")
		}

	case v1alpha1.VolumeContentsTypeCloneVolume:
		if volume.Spec.Contents.CloneVolume == nil || volume.Spec.Contents.CloneSnapshot != nil {
			return ctrl.Result{}, errors.NewBadRequest("invalid volume contents")
		}

	case v1alpha1.VolumeContentsTypeCloneSnapshot:
		if volume.Spec.Contents.CloneSnapshot == nil || volume.Spec.Contents.CloneVolume != nil {
			return ctrl.Result{}, errors.NewBadRequest("invalid volume contents")
		}
	}

	err = r.reconcileNotDeleting(ctx, blobMgr, volume)
	if watch, ok := err.(*util.WatchPending); ok {
		log.Info("reconcile waiting for Watch", "why", watch.Why)
		err = nil // wait until Watch triggers
	}
	return ctrl.Result{}, err
}

func (r *VolumeReconciler) statusUpdate(ctx context.Context, volume *v1alpha1.Volume) error {
	volume.Status.ObservedGeneration = volume.Generation
	return r.Status().Update(ctx, volume)
}
