// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/manager/common/blobs"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
)

type SnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpSnapshotReconciler(mgr ctrl.Manager) error {
	r := &SnapshotReconciler{
		Client: client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		For(&v1alpha1.Snapshot{}).
		Owns(&v1alpha1.ThinPoolLv{}, builder.MatchEveryOwner). // for ThinBlobManager
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots/finalizers,verbs=update,namespace=kubesan-system

func (r *SnapshotReconciler) reconcileDeleting(ctx context.Context, blobMgr blobs.BlobManager, snapshot *v1alpha1.Snapshot) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if err := blobMgr.RemoveBlob(ctx, snapshot.Name, snapshot); err != nil {
		return err
	}

	log.Info("RemoveBlob succeeded")

	if controllerutil.RemoveFinalizer(snapshot, config.Finalizer) {
		if err := r.Update(ctx, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (r *SnapshotReconciler) reconcileNotDeleting(ctx context.Context, blobMgr blobs.BlobManager, snapshot *v1alpha1.Snapshot) error {
	// add finalizer

	if controllerutil.AddFinalizer(snapshot, config.Finalizer) {
		if err := r.Update(ctx, snapshot); err != nil {
			return err
		}
	}

	// create LVM LV if necessary

	if !meta.IsStatusConditionTrue(snapshot.Status.Conditions, v1alpha1.SnapshotConditionAvailable) {
		log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

		err := blobMgr.SnapshotBlob(ctx, snapshot.Name, snapshot.Spec.SourceVolume, snapshot)
		if err != nil {
			return err
		}

		log.Info("SnapshotBlob succeeded")

		condition := metav1.Condition{
			Type:    v1alpha1.SnapshotConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Created",
			Message: "lvm logical snapshot created",
		}
		meta.SetStatusCondition(&snapshot.Status.Conditions, condition)

		sizeBytes, err := blobMgr.GetSize(ctx, snapshot.Name)
		if err != nil {
			return err
		}
		snapshot.Status.SizeBytes = sizeBytes

		if err := r.statusUpdate(ctx, snapshot); err != nil {
			return err
		}
	}

	return nil
}

func (r *SnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("SnapshotReconciler entered")
	defer log.Info("SnapshotReconciler exited")

	snapshot := &v1alpha1.Snapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("SnapshotReconciler NewThinBlobManager", "name", snapshot.Name)

	// This is a hack: the CSI gRPC handler doesn't have direct access to
	// the VG name and the ThinBlobManager doesn't use the VG name for
	// snapshot creation/deletion, so an empty string can be passed here.
	vgName := ""

	blobMgr := blobs.NewThinBlobManager(r.Client, r.Scheme, vgName, snapshot.Spec.SourceVolume)

	var err error
	if snapshot.DeletionTimestamp != nil {
		err = r.reconcileDeleting(ctx, blobMgr, snapshot)
	} else {
		err = r.reconcileNotDeleting(ctx, blobMgr, snapshot)
	}

	if watch, ok := err.(*util.WatchPending); ok && snapshot.DeletionTimestamp == nil {
		log.Info("SnapshotReconciler waiting for Watch", "why", watch.Why)
		err = nil
	}

	return ctrl.Result{}, err
}

func (r *SnapshotReconciler) statusUpdate(ctx context.Context, snapshot *v1alpha1.Snapshot) error {
	snapshot.Status.ObservedGeneration = snapshot.Generation
	return r.Status().Update(ctx, snapshot)
}
