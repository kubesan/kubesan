// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
	"gitlab.com/kubesan/kubesan/internal/manager/common/blobs"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
	"gitlab.com/kubesan/kubesan/internal/manager/common/workers"
)

type VolumeReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	workers *workers.Workers
}

func SetUpVolumeReconciler(mgr ctrl.Manager) error {
	r := &VolumeReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		workers: workers.NewWorkers(),
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Volume{}).
		Owns(&v1alpha1.ThinPoolLv{}, builder.MatchEveryOwner) // for ThinBlobManager
	r.workers.SetUpReconciler(b)
	return b.Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/finalizers,verbs=update,namespace=kubesan-system

func (r *VolumeReconciler) newBlobManager(volume *v1alpha1.Volume) (blobs.BlobManager, error) {
	switch volume.Spec.Mode {
	case v1alpha1.VolumeModeThin:
		return blobs.NewThinBlobManager(r.Client, r.Scheme, volume.Spec.VgName, volume.Name), nil
	case v1alpha1.VolumeModeLinear:
		return blobs.NewLinearBlobManager(r.workers, volume.Spec.VgName), nil
	default:
		return nil, errors.NewBadRequest("invalid volume mode")
	}
}

func (r *VolumeReconciler) reconcileDeleting(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if len(volume.Status.AttachedToNodes) > 0 {
		log.Info("reconcileDeleting waiting for AttachedToNodes[] to become empty")
		return nil // wait until no longer attached
	}

	if err := blobMgr.RemoveBlob(ctx, volume.Name); err != nil {
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

func (r *VolumeReconciler) reconcileNotDeleting(ctx context.Context, blobMgr blobs.BlobManager, volume *v1alpha1.Volume) error {
	// add finalizer

	if controllerutil.AddFinalizer(volume, config.Finalizer) {
		if err := r.Update(ctx, volume); err != nil {
			return err
		}
	}

	// create LVM LV if necessary

	if !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionAvailable) {
		log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

		if err := blobMgr.CreateBlob(ctx, volume.Name, volume.Spec.SizeBytes, volume); err != nil {
			return err
		}

		log.Info("CreateBlob succeeded")

		condition := metav1.Condition{
			Type:    v1alpha1.VolumeConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Created",
			Message: "lvm logical volume created",
		}
		meta.SetStatusCondition(&volume.Status.Conditions, condition)

		volume.Status.SizeBytes = volume.Spec.SizeBytes // TODO report actual size?

		volume.Status.Path = blobMgr.GetPath(volume.Name)

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

	if kubesanslices.CountNonNil(
		volume.Spec.Contents.Empty,
		volume.Spec.Contents.CloneVolume,
		volume.Spec.Contents.CloneSnapshot,
	) != 1 {
		return ctrl.Result{}, errors.NewBadRequest("invalid volume contents")
	}

	switch {
	case volume.Spec.Contents.Empty != nil:
		// nothing to do

	case volume.Spec.Contents.CloneVolume != nil:
		return ctrl.Result{}, errors.NewBadRequest("cloning volumes is not yet supported")

	case volume.Spec.Contents.CloneSnapshot != nil:
		return ctrl.Result{}, errors.NewBadRequest("cloning snapshots is not yet supported")
	}

	err = r.reconcileNotDeleting(ctx, blobMgr, volume)
	if _, ok := err.(*util.WatchPending); ok {
		log.Info("reconcile waiting for Watch")
		err = nil // wait until Watch triggers
	}
	return ctrl.Result{}, err
}

func (r *VolumeReconciler) statusUpdate(ctx context.Context, volume *v1alpha1.Volume) error {
	volume.Status.ObservedGeneration = volume.Generation
	return r.Status().Update(ctx, volume)
}
