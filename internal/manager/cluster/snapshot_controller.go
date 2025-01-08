// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpSnapshotReconciler(mgr ctrl.Manager) error {
	r := &SnapshotReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Snapshot{}).
		Owns(&v1alpha1.ThinPoolLv{}, builder.MatchEveryOwner). // for ThinBlobManager
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots/status,verbs=get;update;patch,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=snapshots/finalizers,verbs=update,namespace=kubesan-system

func (r *SnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, errors.NewBadRequest("not implemented") // TODO
}
