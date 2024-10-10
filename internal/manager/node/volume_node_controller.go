// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
)

type VolumeNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpVolumeNodeReconciler(mgr ctrl.Manager) error {
	r := &VolumeNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Volume{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system

func (r *VolumeNodeReconciler) reconcileLinear(ctx context.Context, volume *v1alpha1.Volume) error {
	// check if already created

	if !conditionsv1.IsStatusConditionTrue(volume.Status.Conditions, conditionsv1.ConditionAvailable) {
		return nil
	}

	shouldBeActive := volume.DeletionTimestamp == nil && slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName)
	isActiveInStatus := slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName)

	path := fmt.Sprintf("/dev/%s/%s", volume.Spec.VgName, volume.Name)
	isActuallyActive, err := commands.PathExistsOnHost(path)
	if err != nil {
		return err
	}

	if shouldBeActive && !isActuallyActive {
		// activate LVM LV on local node

		_, err := commands.Lvm(
			"lvchange",
			"--devicesfile", volume.Spec.VgName,
			"--activate", "sy",
			fmt.Sprintf("%s/%s", volume.Spec.VgName, volume.Name),
		)
		if err != nil {
			return err
		}
		isActuallyActive = true
	} else if !shouldBeActive && isActuallyActive {
		// deactivate LVM LV from local node

		_, err := commands.Lvm(
			"lvchange",
			"--devicesfile", volume.Spec.VgName,
			"--activate", "n",
			fmt.Sprintf("%s/%s", volume.Spec.VgName, volume.Name),
		)
		if err != nil {
			return err
		}
		isActuallyActive = false
	}

	// update status to reflect reality if necessary

	if !isActiveInStatus && isActuallyActive {
		volume.Status.AttachedToNodes = append(volume.Status.AttachedToNodes, config.LocalNodeName)
	} else if isActiveInStatus && !isActuallyActive {
		volume.Status.AttachedToNodes = kubesanslices.RemoveAll(volume.Status.AttachedToNodes, config.LocalNodeName)
	} else {
		return nil // done, no need to update Status
	}

	err = r.Status().Update(ctx, volume)
	return err
}

func (r *VolumeNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// TODO: Avoid running while the cluster-wide Volume controller or another instance of the node-local Volume
	// controller is reconciling the same Volume, OR make sure that there are no races.

	volume := &v1alpha1.Volume{}
	if err := r.Get(ctx, req.NamespacedName, volume); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var err error

	switch volume.Spec.Mode {
	case v1alpha1.VolumeModeThin:
		err = errors.NewBadRequest("not implemented") // TODO
	case v1alpha1.VolumeModeLinear:
		err = r.reconcileLinear(ctx, volume)
	default:
		err = errors.NewBadRequest("invalid volume mode")
	}

	return ctrl.Result{}, err
}
