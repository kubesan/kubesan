// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
		Client: client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		Scheme: mgr.GetScheme(),
	}

	nbd.Startup()

	// We are only interested in NBDExports on this node; set up
	// an indexer so that we can filter lists.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.NBDExport{}, "spec.host", func(rawObj client.Object) []string {
		export := rawObj.(*v1alpha1.NBDExport)
		return []string{export.Spec.Host}
	}); err != nil {
		return err
	}

	// Time for some magic.  The default ctrl.SetupSignalHandler
	// points to signals.SetupSignalHandler() that does not let us
	// inject any cleanup between the arrival of SIGTERM and when
	// it starts cancelling all controllers.  But we can replace
	// it with our own variant that lets us delay the cancel()
	// after SIGTERM is first detected until we are satisfied that
	// we are not stranding any active NBDExports.  Note that if
	// our efforts for a clean shutdown fail to act quickly,
	// kubelet will still force an exit after the pod's
	// terminationGracePeriodSeconds.
	ctrl.SetupSignalHandler = func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		log := ctrl.Log.WithName("shutdown")
		ctx = ctrl.LoggerInto(ctx, log)
		c := make(chan os.Signal, 2)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		log.Info("shutdown context created")
		go func() {
			<-c
			log.Info("signal received, starting shutdown")
			done := nbd.StopServer(ctx)
			if err := triggerCleanup(ctx, r.Client); err != nil {
				log.Error(err, "safe shutdown failed")
			}
			select {
			case <-done:
				log.Info("shutdown proceeding to cancel context")
				cancel()
				<-c
			case <-c:
			}
			log.Info("shutdown got second signal")
			os.Exit(1) // Exit directly on second signal.
		}()

		return ctx
	}

	// Now to create the controller.

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		// We are only interested in Spec changes to the
		// NBDExports hosted on this node.
		For(&v1alpha1.NBDExport{}, builder.WithPredicates(predicate.And(predicate.NewPredicateFuncs(func(object client.Object) bool {
			export := object.(*v1alpha1.NBDExport)
			return export.Spec.Host == config.LocalNodeName
		}), predicate.GenerationChangedPredicate{}))).
		Complete(r)
}

// Inform all NBDExports on this node that SIGTERM has been seen.
func triggerCleanup(ctx context.Context, c client.Client) error {
	log := log.FromContext(ctx)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nbdExportList := &v1alpha1.NBDExportList{}
		if err := c.List(ctx, nbdExportList, client.MatchingFields{"spec.host": config.LocalNodeName}); err != nil {
			return err
		}
		var lastErr error = nil
		for i := range nbdExportList.Items {
			export := &nbdExportList.Items[i]
			if export.Spec.Host != config.LocalNodeName || export.Status.URI == "" || export.Spec.Path == "" {
				continue
			}
			log.Info("triggering export shutdown", "export", export.Name)
			export.Spec.Path = ""
			if err := c.Update(ctx, export); client.IgnoreNotFound(err) != nil {
				lastErr = err
			}
		}
		return lastErr
	})
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

	if export.Spec.Path == "" && meta.IsStatusConditionTrue(export.Status.Conditions, v1alpha1.NBDExportConditionAvailable) {
		// Set condition["available"] to false only if it was true
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

	if export.Status.URI == "" && !nbd.Shutdown {
		log.Info("Starting NBD export")

		uri, err := nbd.StartExport(ctx, export.Spec.Export, export.Spec.Path)
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

	if len(export.Spec.Clients) > 0 {
		log.Info("Checking NBD export status")
		if err := nbd.CheckExportHealth(ctx, export.Spec.Export); err != nil {
			// Don't check prior state of condition["available"],
			// because this Reason takes priority even if the
			// export had already started clean shutdown
			condition := metav1.Condition{
				Type:    v1alpha1.NBDExportConditionAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "DeviceError",
				Message: "unexpected NBD server error",
			}
			if meta.SetStatusCondition(&export.Status.Conditions, condition) {
				if err := r.statusUpdate(ctx, export); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, err
		}
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

	if err := nbd.StopExport(ctx, export.Spec.Export); err != nil {
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
