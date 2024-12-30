// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/common/nbd"
)

type NBDExportNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpNBDExportNodeReconciler(mgr ctrl.Manager) error {
	r := &NBDExportNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.NBDExport{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=nbdexports,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=nbdexports/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=nbdexports/finalizers,verbs=update,namespace=kubesan-system

func (r *NBDExportNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("node", config.LocalNodeName)

	log.Info("NBDExportNodeReconciler entered")
	defer log.Info("NBDExportNodeReconciler exited")

	export := &v1alpha1.NBDExport{}
	if err := r.Get(ctx, req.NamespacedName, export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if export.Spec.Host != config.LocalNodeName {
		return ctrl.Result{}, nil
	}

	if export.DeletionTimestamp != nil {
		log.Info("Attempting deletion")
		err := r.reconcileDeleting(ctx, export)
		return ctrl.Result{}, err
	}

	if controllerutil.AddFinalizer(export, config.Finalizer) {
		if err := r.Update(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	if export.Spec.Path == "" {
		condition := metav1.Condition{
			Type:    v1alpha1.NBDExportConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  "Stopping",
			Message: "server stop requested, waiting for clients to disconnect",
		}
		meta.SetStatusCondition(&export.Status.Conditions, condition)
		if err := r.statusUpdate(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	serverId := &nbd.ServerId{
		Node:   config.LocalNodeName,
		Export: export.Spec.Export,
	}

	if export.Status.URI == "" {
		log.Info("Starting NBD export")

		uri, err := nbd.StartServer(ctx, serverId, export.Spec.Path)
		if err != nil {
			return ctrl.Result{}, err
		}
		export.Status.URI = uri
		condition := metav1.Condition{
			Type:    v1alpha1.NBDExportConditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Ready",
			Message: "NBD Export is ready",
		}
		meta.SetStatusCondition(&export.Status.Conditions, condition)
		if err = r.statusUpdate(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("Checking NBD export status")
	if err := nbd.CheckServerHealth(ctx, serverId); err != nil {
		condition := metav1.Condition{
			Type:    v1alpha1.NBDExportConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  "DeviceError",
			Message: "unexpected NBD server error",
		}
		meta.SetStatusCondition(&export.Status.Conditions, condition)
		if err := r.statusUpdate(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *NBDExportNodeReconciler) reconcileDeleting(ctx context.Context, export *v1alpha1.NBDExport) error {
	// Mark the export unavailable, so no new clients attach
	if !nbd.ExportDegraded(export) {
		condition := metav1.Condition{
			Type:    v1alpha1.NBDExportConditionAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  "Deleting",
			Message: "deletion requested, waiting for clients to disconnect",
		}
		meta.SetStatusCondition(&export.Status.Conditions, condition)
		if err := r.statusUpdate(ctx, export); err != nil {
			return err
		}
	}

	// Wait for all existing clients to detach
	if len(export.Spec.Clients) > 0 {
		return nil // wait until no longer attached
	}

	serverId := &nbd.ServerId{
		Node:   config.LocalNodeName,
		Export: export.Spec.Export,
	}
	if err := nbd.StopServer(ctx, serverId); err != nil {
		return err
	}

	// Now the CR can be deleted
	controllerutil.RemoveFinalizer(export, config.Finalizer)
	return r.Update(ctx, export)
}

func (r *NBDExportNodeReconciler) statusUpdate(ctx context.Context, export *v1alpha1.NBDExport) error {
	export.Status.ObservedGeneration = export.Generation
	return r.Status().Update(ctx, export)
}
