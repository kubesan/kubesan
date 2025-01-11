// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/common/dm"
	"gitlab.com/kubesan/kubesan/internal/common/nbd"
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
		Owns(&v1alpha1.NBDExport{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system

// Ensure that the volume is attached to this node.
// May fail with WatchPending if another reconcile will trigger progress.
// Triggered by CSI NodeStageVolume.
func (r *VolumeNodeReconciler) reconcileThinAttaching(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	oldThinPoolLv := thinPoolLv.DeepCopy()
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if err := dm.Create(ctx, volume.Name, volume.Spec.SizeBytes); err != nil {
		return err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	if thinLvSpec == nil || thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameRemoved {
		return errors.NewBadRequest("unexpected missing blob")
	} else {
		thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameActive
	}

	// Update the ThinPool activation claims.
	thinPoolLv.Spec.ActiveOnNode = volume.Spec.AttachToNodes[0]
	if err := thinpoollv.UpdateThinPoolLv(ctx, r.Client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}

	var device string
	if thinPoolLv.Spec.ActiveOnNode == config.LocalNodeName {
		// This node claimed activation rights, make sure the claim
		// is honored before actually resuming the dm
		log.Info("Attaching on local node")
		if !isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
			return &util.WatchPending{}
		}
		device = devName(volume)
	} else {
		// Another node claimed activation rights, make sure there
		// is an NBD export, and connect to it before resuming the dm
		log.Info("Attaching via NBD client")
		if volume.Status.NBDExport == "" {
			return &util.WatchPending{}
		}
		nbdExport := &v1alpha1.NBDExport{}
		err := r.Get(ctx, types.NamespacedName{Name: volume.Status.NBDExport, Namespace: config.Namespace}, nbdExport)
		if client.IgnoreNotFound(err) != nil {
			return err
		}

		device, err = nbd.ConnectClient(ctx, r.Client, nbdExport)
		if err != nil {
			return err
		}
	}
	log.Info("Device ready to attach", "device", device)
	return dm.Resume(ctx, volume.Name, volume.Spec.SizeBytes, device)
}

// Ensure that the volume is detached from this node.
// Triggered by CSI NodeUnstageVolume.
func (r *VolumeNodeReconciler) reconcileThinDetaching(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	oldThinPoolLv := thinPoolLv.DeepCopy()

	if err := dm.Remove(ctx, volume.Name); err != nil {
		return err
	}

	if thinPoolLv.Status.ActiveOnNode != config.LocalNodeName && thinPoolLv.Spec.ActiveOnNode != config.LocalNodeName {
		return nil // it's not attached to this node
	}

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	// TODO: Do not mark this LV inactive if there is still some other
	// snapshot being created in the same thin pool
	if len(volume.Spec.AttachToNodes) == 0 {
		if thinLvSpec != nil && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
			thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameInactive
		}
	} else {
		thinPoolLv.Spec.ActiveOnNode = volume.Spec.AttachToNodes[0]
	}

	if err := thinpoollv.UpdateThinPoolLv(ctx, r.Client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}
	if thinPoolLv.Status.ActiveOnNode == config.LocalNodeName {
		return &util.WatchPending{}
	}
	return nil
}

// Handle any NBD client or server that needs to be cleaned up before
// detaching or migrating a node activation.
func (r *VolumeNodeReconciler) reconcileThinNBDCleanup(ctx context.Context, volume *v1alpha1.Volume) error {
	nbdExport := &v1alpha1.NBDExport{}
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if volume.Status.NBDExport == "" {
		return nil
	}
	err := r.Get(ctx, types.NamespacedName{Name: volume.Status.NBDExport, Namespace: config.Namespace}, nbdExport)
	if errors.IsNotFound(err) {
		volume.Status.NBDExport = ""
		err = r.statusUpdate(ctx, volume)
	}
	if err != nil {
		return err
	}

	connected, disconnect := nbd.CheckClient(ctx, r.Client, nbdExport)
	if (connected && !slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName)) || disconnect {
		log.Info("Cleaning up NBD client", "export", volume.Status.NBDExport)
		if err := dm.Suspend(ctx, volume.Name, volume.Spec.Type.Block != nil); err != nil {
			return err
		}
		if err := nbd.DisconnectClient(ctx, r.Client, nbdExport); err != nil {
			return err
		}
	}

	if nbd.ShouldStopExport(nbdExport, volume.Spec.AttachToNodes) {
		log.Info("Cleaning up NBD server", "export", volume.Status.NBDExport)
		if nbdExport.Spec.Path != "" {
			nbdExport.Spec.Path = ""
			if err := r.Update(ctx, nbdExport); err != nil {
				return err
			}
		}
		if len(nbdExport.Spec.Clients) == 0 {
			propagation := client.PropagationPolicy(metav1.DeletePropagationBackground)
			if err := r.Delete(ctx, nbdExport, propagation); err != nil && !errors.IsNotFound(err) {
				return err
			}
			volume.Status.NBDExport = ""
			if err := r.statusUpdate(ctx, volume); err != nil {
				return err
			}
		} else {
			return &util.WatchPending{}
		}
	}
	return nil
}

// Handle the creation of any NBD export
func (r *VolumeNodeReconciler) reconcileThinNBDSetup(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	nodes := volume.Spec.AttachToNodes
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if !isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
		return nil
	}
	if !slices.Contains(nodes, config.LocalNodeName) || len(nodes) == 1 {
		return nil
	}

	name := config.LocalNodeName + "-" + thinpoollv.VolumeToThinLvName(volume.Name)
	if volume.Status.NBDExport == name {
		return nil
	}

	log.Info("setting up NBD export")
	export := &v1alpha1.NBDExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
		},
		Spec: v1alpha1.NBDExportSpec{
			Host:   config.LocalNodeName,
			Export: volume.Name,
			Path:   devName(volume),
		},
	}
	controllerutil.AddFinalizer(export, config.Finalizer)
	if err := controllerutil.SetControllerReference(volume, export, r.Scheme); err != nil {
		return err
	}

	if err := client.IgnoreAlreadyExists(r.Client.Create(ctx, export)); err != nil {
		return err
	}

	volume.Status.NBDExport = name
	return r.statusUpdate(ctx, volume)
}

func isThinLvActiveOnLocalNode(thinPoolLv *v1alpha1.ThinPoolLv, name string) bool {
	thinLvStatus := thinPoolLv.Status.FindThinLv(name)
	return thinLvStatus != nil && thinPoolLv.Status.ActiveOnNode == config.LocalNodeName && thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameActive
}

// Update Volume.Status.AttachedToNodes[] from the ThinPoolLv.
func (r *VolumeNodeReconciler) updateStatusAttachedToNodes(ctx context.Context, volume *v1alpha1.Volume, attached bool) error {
	if attached {
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

	err := r.Get(ctx, types.NamespacedName{Name: volume.Name, Namespace: config.Namespace}, thinPoolLv)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if err = r.reconcileThinNBDCleanup(ctx, volume); err != nil {
		return err
	}

	var attached bool
	if slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName) {
		err = r.reconcileThinAttaching(ctx, volume, thinPoolLv)
		attached = true
	} else {
		err = r.reconcileThinDetaching(ctx, volume, thinPoolLv)
		attached = false
	}

	if err != nil {
		return err
	}

	if err := r.updateStatusAttachedToNodes(ctx, volume, attached); err != nil {
		return err
	}

	if err := r.reconcileThinNBDSetup(ctx, volume, thinPoolLv); err != nil {
		return err
	}

	return nil
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

	if _, ok := err.(*util.WatchPending); ok {
		log.Info("reconcile waiting for Watch")
		return ctrl.Result{}, nil // wait until Watch triggers
	}

	return ctrl.Result{}, err
}

func (r *VolumeNodeReconciler) statusUpdate(ctx context.Context, volume *v1alpha1.Volume) error {
	volume.Status.ObservedGeneration = volume.Generation
	return r.Status().Update(ctx, volume)
}
