// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
)

type VolumeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpVolumeReconciler(mgr ctrl.Manager) error {
	r := &VolumeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Volume{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/finalizers,verbs=update,namespace=kubesan-system

func newBlobManager(volume *v1alpha1.Volume) (BlobManager, error) {
	switch volume.Spec.Mode {
	case v1alpha1.VolumeModeThin:
		return nil, errors.NewBadRequest("thin blobs not yet implemented")
	case v1alpha1.VolumeModeLinear:
		return NewLinearBlobManager(volume.Spec.VgName), nil
	default:
		return nil, errors.NewBadRequest("invalid volume mode")
	}
}

func (r *VolumeReconciler) reconcileDeleting(ctx context.Context, blobMgr BlobManager, volume *v1alpha1.Volume) error {
	if len(volume.Status.AttachedToNodes) > 0 {
		return nil // wait until no longer attached
	}

	if err := blobMgr.RemoveBlob(volume.Name); err != nil {
		return err
	}

	controllerutil.RemoveFinalizer(volume, config.Finalizer)

	if err := r.Update(ctx, volume); err != nil {
		return err
	}
	return nil
}

func (r *VolumeReconciler) reconcileNotDeleting(ctx context.Context, blobMgr BlobManager, volume *v1alpha1.Volume) error {
	// add finalizer

	if !controllerutil.ContainsFinalizer(volume, config.Finalizer) {
		controllerutil.AddFinalizer(volume, config.Finalizer)

		if err := r.Update(ctx, volume); err != nil {
			return err
		}
	}

	// create LVM LV if necessary

	if !conditionsv1.IsStatusConditionTrue(volume.Status.Conditions, conditionsv1.ConditionAvailable) {
		err := blobMgr.CreateBlob(volume.Name, volume.Spec.SizeBytes)
		if err != nil {
			return err
		}

		condition := conditionsv1.Condition{
			Type:   conditionsv1.ConditionAvailable,
			Status: corev1.ConditionTrue,
		}
		conditionsv1.SetStatusCondition(&volume.Status.Conditions, condition)

		volume.Status.SizeBytes = volume.Spec.SizeBytes // TODO report actual size?

		path := fmt.Sprintf("/dev/%s/%s", volume.Spec.VgName, volume.Name)
		volume.Status.Path = &path

		if err := r.Status().Update(ctx, volume); err != nil {
			return err
		}
	}

	return nil
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// log := log.FromContext(ctx)

	volume := &v1alpha1.Volume{}
	if err := r.Get(ctx, req.NamespacedName, volume); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	blobMgr, err := newBlobManager(volume)
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

	switch {
	case volume.Spec.Type.Block != nil:
		// nothing to do

	case volume.Spec.Type.Filesystem != nil:
		return ctrl.Result{}, errors.NewBadRequest("not implemented") // TODO
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

	return ctrl.Result{}, r.reconcileNotDeleting(ctx, blobMgr, volume)
}
