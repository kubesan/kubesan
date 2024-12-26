// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/common/dm"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
	"gitlab.com/kubesan/kubesan/internal/manager/common/thinpoollv"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
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
		Owns(&v1alpha1.ThinPoolLv{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system

// Ensure that the volume is attached to this node
// May fail with WatchPending if another reconcile will trigger progress
func (r *VolumeNodeReconciler) reconcileThinAttaching(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	oldThinPoolLv := thinPoolLv.DeepCopy()
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if thinPoolLv.Spec.ActiveOnNode != "" && thinPoolLv.Spec.ActiveOnNode != config.LocalNodeName {
		log.Info("Attaching to this node but already active on another node", "Spec.ActiveOnNode", thinPoolLv.Spec.ActiveOnNode)
		return errors.NewBadRequest("attaching via NBD not yet implemented")
	}
	thinPoolLv.Spec.ActiveOnNode = config.LocalNodeName

	if err := dm.Create(ctx, volume.Name, volume.Spec.SizeBytes); err != nil {
		return err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	if thinLvSpec != nil && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameInactive {
		thinLvSpec.State = v1alpha1.ThinLvSpecState{
			Name: v1alpha1.ThinLvSpecStateNameActive,
		}
	}

	if err := thinpoollv.UpdateThinPoolLv(ctx, r.Client, thinPoolLv, oldThinPoolLv != thinPoolLv); err != nil {
		return err
	}

	if !isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
		return &util.WatchPending{}
	}

	return dm.Resume(ctx, volume.Name, volume.Spec.SizeBytes, devName(volume))
}

// Ensure that the volume is detached from this node
func (r *VolumeNodeReconciler) reconcileThinDetaching(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	oldThinPoolLv := thinPoolLv.DeepCopy()

	if thinPoolLv.Status.ActiveOnNode != config.LocalNodeName {
		if thinPoolLv.Spec.ActiveOnNode == config.LocalNodeName {
			// clear thinPoolLv.Spec.ActiveOnNode once activation is no longer required
			return thinpoollv.UpdateThinPoolLv(ctx, r.Client, thinPoolLv, false)
		}
		return nil // it's not attached to this node
	}

	if err := dm.Remove(ctx, volume.Name); err != nil {
		return err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	if thinLvSpec != nil && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
		thinLvSpec.State = v1alpha1.ThinLvSpecState{
			Name: v1alpha1.ThinLvSpecStateNameInactive,
		}
	}

	return thinpoollv.UpdateThinPoolLv(ctx, r.Client, thinPoolLv, oldThinPoolLv != thinPoolLv)
}

func isThinLvActiveOnLocalNode(thinPoolLv *v1alpha1.ThinPoolLv, name string) bool {
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)
	return thinLvStatus != nil && thinPoolLv.Status.ActiveOnNode == config.LocalNodeName && thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameActive
}

// Update Volume.Status.AttachedToNodes[] from the ThinPoolLv
func (r *VolumeNodeReconciler) updateStatusAttachedToNodes(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)

	if isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
		if !slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName) {
			volume.Status.AttachedToNodes = append(volume.Status.AttachedToNodes, config.LocalNodeName)

			if err := r.statusUpdate(ctx, volume); err != nil {
				return err
			}
		}
	} else {
		if slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName) {
			volume.Status.AttachedToNodes = kubesanslices.RemoveAll(volume.Status.AttachedToNodes, config.LocalNodeName)

			if err := r.statusUpdate(ctx, volume); err != nil {
				return err
			}
		}
	}

	return nil
}

func devName(volume *v1alpha1.Volume) string {
	return "/dev/" + volume.Spec.VgName + "/" + thinpoollv.VolumeToThinLvName(volume.Name)
}

func (r *VolumeNodeReconciler) reconcileThin(ctx context.Context, volume *v1alpha1.Volume) error {
	thinPoolLv := &v1alpha1.ThinPoolLv{}
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	err := r.Get(ctx, types.NamespacedName{Name: volume.Name, Namespace: config.Namespace}, thinPoolLv)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName) {
		err = r.reconcileThinAttaching(ctx, volume, thinPoolLv)
	} else {
		err = r.reconcileThinDetaching(ctx, volume, thinPoolLv)
	}

	if err != nil {
		if _, ok := err.(*util.WatchPending); ok {
			log.Info("reconcileThin waiting for Watch")
			return nil // wait until Watch triggers
		}
		return err
	}

	return r.updateStatusAttachedToNodes(ctx, volume, thinPoolLv)
}

func (r *VolumeNodeReconciler) reconcileLinear(ctx context.Context, volume *v1alpha1.Volume) error {
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

	err = r.statusUpdate(ctx, volume)
	return err
}

func (r *VolumeNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// TODO: Avoid running while the cluster-wide Volume controller or another instance of the node-local Volume
	// controller is reconciling the same Volume, OR make sure that there are no races.

	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("VolumeNodeReconciler entered")
	defer log.Info("VolumeNodeReconciler exited")

	volume := &v1alpha1.Volume{}
	if err := r.Get(ctx, req.NamespacedName, volume); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// check if already created

	if !meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionAvailable) {
		return ctrl.Result{}, nil
	}

	var err error

	switch volume.Spec.Mode {
	case v1alpha1.VolumeModeThin:
		err = r.reconcileThin(ctx, volume)
	case v1alpha1.VolumeModeLinear:
		err = r.reconcileLinear(ctx, volume)
	default:
		err = errors.NewBadRequest("invalid volume mode")
	}

	return ctrl.Result{}, err
}

func (r *VolumeNodeReconciler) statusUpdate(ctx context.Context, volume *v1alpha1.Volume) error {
	volume.Status.ObservedGeneration = volume.Generation
	return r.Status().Update(ctx, volume)
}
