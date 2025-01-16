// SPDX-License-Identifier: Apache-2.0

// The ThinPoolLv cluster controller creates and removes thin-pools. Everything
// else is handled by the node controller.

package cluster

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

type ThinPoolLvReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpThinPoolLvReconciler(mgr ctrl.Manager) error {
	r := &ThinPoolLvReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ThinPoolLv{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=thinpoollvs,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=thinpoollvs/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=thinpoollvs/finalizers,verbs=update,namespace=kubesan-system

func (r *ThinPoolLvReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("ThinPoolLvReconciler entered")
	defer log.Info("ThinPoolLvReconciler exited")

	thinPoolLv := &v1alpha1.ThinPoolLv{}
	if err := r.Get(ctx, req.NamespacedName, thinPoolLv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var err error

	if thinPoolLv.DeletionTimestamp == nil {
		err = r.reconcileNotDeleting(ctx, thinPoolLv)
	} else {
		err = r.reconcileDeleting(ctx, thinPoolLv)
	}

	return ctrl.Result{}, err
}

func (r *ThinPoolLvReconciler) reconcileNotDeleting(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	// add finalizer

	if controllerutil.AddFinalizer(thinPoolLv, config.Finalizer) {
		if err := r.Update(ctx, thinPoolLv); err != nil {
			return err
		}
	}

	// create LVM thin pool LV

	if !meta.IsStatusConditionTrue(thinPoolLv.Status.Conditions, v1alpha1.ThinPoolLvConditionAvailable) {
		err := r.createThinPoolLv(ctx, thinPoolLv)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *ThinPoolLvReconciler) reconcileDeleting(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	// wait for node controller to deactivate thin-pool so there are no races

	if meta.IsStatusConditionTrue(thinPoolLv.Status.Conditions, v1alpha1.ThinPoolLvConditionActive) {
		return nil
	}

	// remove LVM thin LVs

	for i := range thinPoolLv.Status.ThinLvs {
		thinLv := &thinPoolLv.Status.ThinLvs[i]

		if thinLv.State.Name != v1alpha1.ThinLvStatusStateNameInactive && thinLv.State.Name != v1alpha1.ThinLvStatusStateNameRemoved {
			return nil // try again when the LV becomes inactive
		}

		err := r.removeThinLv(thinPoolLv, thinLv.Name)
		if err != nil {
			return err
		}
	}

	if len(thinPoolLv.Status.ThinLvs) > 0 {
		thinPoolLv.Status.ThinLvs = []v1alpha1.ThinLvStatus{}
		if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
			return err
		}
	}

	// remove LVM thin pool LV

	err := r.removeThinPoolLv(ctx, thinPoolLv)
	if err != nil {
		return err
	}

	return nil
}

func (r *ThinPoolLvReconciler) createThinPoolLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	_, err := commands.LvmLvCreateIdempotent(
		"--devicesfile", thinPoolLv.Spec.VgName,
		"--activate", "n",
		"--type", "thin-pool",
		"--metadataprofile", config.LvmProfileName,
		"--name", thinPoolLv.Name,
		"--size", fmt.Sprintf("%db", thinPoolLv.Spec.SizeBytes),
		thinPoolLv.Spec.VgName,
	)
	if err != nil {
		return err
	}

	condition := metav1.Condition{
		Type:    v1alpha1.ThinPoolLvConditionAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  "Created",
		Message: "thin pool logical volume created",
	}
	meta.SetStatusCondition(&thinPoolLv.Status.Conditions, condition)

	if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
		return err
	}

	return nil
}

func (r *ThinPoolLvReconciler) removeThinPoolLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	_, err := commands.LvmLvRemoveIdempotent(
		"--devicesfile", thinPoolLv.Spec.VgName,
		fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinPoolLv.Name),
	)
	if err != nil {
		return err
	}

	if controllerutil.RemoveFinalizer(thinPoolLv, config.Finalizer) {
		if err := r.Update(ctx, thinPoolLv); err != nil {
			return err
		}
	}

	return nil
}

func (r *ThinPoolLvReconciler) removeThinLv(thinPoolLv *v1alpha1.ThinPoolLv, thinLvName string) error {
	_, err := commands.LvmLvRemoveIdempotent(
		"--devicesfile", thinPoolLv.Spec.VgName,
		fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinLvName),
	)
	return err
}

func (r *ThinPoolLvReconciler) statusUpdate(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	thinPoolLv.Status.ObservedGeneration = thinPoolLv.Generation
	return r.Status().Update(ctx, thinPoolLv)
}
