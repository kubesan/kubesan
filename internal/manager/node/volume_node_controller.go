// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	"gitlab.com/kubesan/kubesan/internal/manager/common/workers"
)

type VolumeNodeReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	workers *workers.Workers
}

func SetUpVolumeNodeReconciler(mgr ctrl.Manager) error {
	r := &VolumeNodeReconciler{
		Client:  client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		Scheme:  mgr.GetScheme(),
		workers: workers.NewWorkers(),
	}

	b := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		For(&v1alpha1.Volume{}).
		Owns(&v1alpha1.ThinPoolLv{}, builder.MatchEveryOwner). // for ThinBlobManager
		Owns(&v1alpha1.NBDExport{})
	r.workers.SetUpReconciler(b)
	return b.Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=volumes/status,verbs=get;update;patch,namespace=kubesan-system

// Ensure that the volume is attached to this node.
// May fail with WatchPending if another reconcile will trigger progress.
// Triggered by CSI NodeStageVolume.
func (r *VolumeNodeReconciler) reconcileThinAttaching(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	oldThinPoolLv := thinPoolLv.DeepCopy()
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	if thinLvSpec == nil || thinLvStatus == nil || thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameRemoved {
		return errors.NewBadRequest("unexpected missing blob")
	} else {
		thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameActive
	}

	sizeBytes := thinLvStatus.SizeBytes
	if err := dm.Create(ctx, volume.Name, sizeBytes); err != nil {
		return err
	}

	// Update the ThinPool activation claims.
	thinPoolLv.Spec.ActiveOnNode = thinpoollv.InterestedNodes(ctx, thinPoolLv, volume.Spec.AttachToNodes)[0]
	if err := thinpoollv.UpdateThinPoolLv(ctx, r.Client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}

	var device string
	if thinPoolLv.Spec.ActiveOnNode == config.LocalNodeName {
		// This node claimed activation rights, make sure the claim
		// is honored before actually resuming the dm
		log.Info("Attaching on local node")
		if !isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
			return util.NewWatchPending("waiting for thinlv activation")
		}
		device = devName(volume)
	} else {
		// Another node claimed activation rights, make sure there
		// is an NBD export, and connect to it before resuming the dm
		log.Info("Attaching via NBD client")
		if volume.Status.NBDExport == "" {
			return util.NewWatchPending("waiting for NBD server")
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
		sizeBytes = nbdExport.Spec.SizeBytes
	}
	log.Info("Device ready to attach", "device", device)
	return dm.Resume(ctx, volume.Name, sizeBytes, device)
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
	if len(volume.Spec.AttachToNodes) == 0 && thinLvSpec != nil && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
		thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameInactive
	}
	nodes := thinpoollv.InterestedNodes(ctx, thinPoolLv, volume.Spec.AttachToNodes)
	if len(nodes) == 0 {
		thinPoolLv.Spec.ActiveOnNode = ""
	} else {
		thinPoolLv.Spec.ActiveOnNode = nodes[0]
	}
	if err := thinpoollv.UpdateThinPoolLv(ctx, r.Client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}
	if len(volume.Spec.AttachToNodes) == 0 {
		thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
		if thinLvStatus != nil && thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameActive {
			return util.NewWatchPending("waiting for thinlv deactivation")
		}
	}
	return nil
}

// Handle any NBD client or server that needs to be cleaned up before
// detaching or migrating a node activation.
func (r *VolumeNodeReconciler) reconcileThinNBDCleanup(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv, sizeBytes int64) error {
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

	if nbd.ShouldStopExport(nbdExport, thinpoollv.InterestedNodes(ctx, thinPoolLv, volume.Spec.AttachToNodes), sizeBytes) {
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
			return util.NewWatchPending("waiting for NBD clients to detach")
		}
	}
	return nil
}

// Handle the creation of any NBD export
func (r *VolumeNodeReconciler) reconcileThinNBDSetup(ctx context.Context, volume *v1alpha1.Volume, thinPoolLv *v1alpha1.ThinPoolLv) error {
	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	nodes := thinpoollv.InterestedNodes(ctx, thinPoolLv, volume.Spec.AttachToNodes)
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
			Labels:    config.CommonLabels,
		},
		Spec: v1alpha1.NBDExportSpec{
			Host:      config.LocalNodeName,
			Export:    volume.Name,
			Path:      devName(volume),
			SizeBytes: thinLvStatus.SizeBytes,
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
func (r *VolumeNodeReconciler) updateStatusAttachedToNodes(ctx context.Context, volume *v1alpha1.Volume, attached bool, thinPoolLv *v1alpha1.ThinPoolLv) error {
	needsUpdate := false
	if attached {
		if !slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName) {
			volume.Status.AttachedToNodes = append(volume.Status.AttachedToNodes, config.LocalNodeName)
			needsUpdate = true

		}
		thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
		if isThinLvActiveOnLocalNode(thinPoolLv, thinLvName) {
			thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
			if thinLvStatus.SizeBytes > volume.Status.SizeBytes {
				volume.Status.SizeBytes = thinLvStatus.SizeBytes
				needsUpdate = true
			}
		}
	} else {
		if slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName) {
			volume.Status.AttachedToNodes = kubesanslices.RemoveAll(volume.Status.AttachedToNodes, config.LocalNodeName)
			needsUpdate = true
		}
	}

	if needsUpdate {
		if err := r.statusUpdate(ctx, volume); err != nil {
			return err
		}
	}
	return nil
}

func devName(volume *v1alpha1.Volume) string {
	return "/dev/" + volume.Spec.VgName + "/" + thinpoollv.VolumeToThinLvName(volume.Name)
}

type ddWork struct {
	targetPathOnHost string
	sourcePathOnHost string
}

func (w *ddWork) Run(ctx context.Context) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("Starting to populate new volume", "sourcePathOnHost", w.sourcePathOnHost, "targetPathOnHost", w.targetPathOnHost)
	_, err := commands.RunOnHostContext(
		ctx,
		"dd",
		"if="+w.sourcePathOnHost,
		"of="+w.targetPathOnHost,
		"bs=1M",
		"conv=fsync,nocreat,sparse",
	)
	if err != nil {
		log.Error(err, "dd failed")
		return err
	}

	// To test long-running operations: _, err := commands.RunOnHostContext(ctx, "sleep", "30")
	log.Info("Finished populating new volume", "sourcePathOnHost", w.sourcePathOnHost, "targetPathOnHost", w.targetPathOnHost)
	return nil
}

// Populate an activated destination volume from a snapshot source, and return its size.
func (r *VolumeNodeReconciler) reconcileSourcePopulating(ctx context.Context, volume *v1alpha1.Volume, targetPathOnHost string) (int64, error) {
	if volume.Spec.Contents.ContentsType == v1alpha1.VolumeContentsTypeEmpty {
		return 0, errors.NewBadRequest("unexpected empty source")
	}

	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	workName := "dd-" + volume.Name

	if volume.DeletionTimestamp != nil {
		log.Info("Canceling dd work due to volume deletion", "volumeName", volume.Name)
		return 0, r.workers.Cancel(workName) // stop dd
	}

	// Cross-VG cloning is not supported, so it's safe to use the target Volume's VG
	vgName := volume.Spec.VgName
	sourceLv := thinpoollv.VolumeToThinLvName("clone-" + volume.Name)
	sourcePathOnHost := "/dev/" + vgName + "/" + sourceLv

	// We should only reach here on node where data source is already activated

	if exists, _ := commands.PathExistsOnHost(sourcePathOnHost); !exists {
		log.Info("Source path does not exist on host", "path", sourcePathOnHost)
		return 0, errors.NewBadRequest("unexpected missing source mount")
	}
	if exists, _ := commands.PathExistsOnHost(targetPathOnHost); !exists {
		log.Info("Target path does not exist on host", "path", targetPathOnHost)
		return 0, errors.NewBadRequest("unexpected missing destination mount")
	}

	work := &ddWork{
		sourcePathOnHost: sourcePathOnHost,
		targetPathOnHost: targetPathOnHost,
	}

	err := r.workers.Run(workName, volume, work)
	if err != nil {
		return 0, err
	}
	return commands.LvmSize(vgName, sourceLv)
}

// Populate a Thin volume from a snapshot source.
func (r *VolumeNodeReconciler) reconcileThinPopulating(ctx context.Context, volume *v1alpha1.Volume) error {
	// Empty volumes were done as no-op in cluster reconcile.
	_, err := r.reconcileSourcePopulating(ctx, volume, devName(volume))
	return err
}

type blkdiscardWork struct {
	device string
	offset int64
}

func (w *blkdiscardWork) Run(ctx context.Context) error {
	log := log.FromContext(ctx)
	args := []string{"blkdiscard", "--zeroout", "--offset",
		strconv.FormatInt(w.offset, 10), w.device}
	log.Info("blkdiscard worker zeroing LV", "path", w.device, "command", args)
	_, err := commands.RunOnHostContext(ctx, args...)
	// To test long-running operations: _, err := commands.RunOnHostContext(ctx, "sleep", "30")
	log.Info("blkdiscard worker finished", "path", w.device)
	return err
}

// Returns a unique name for a blkdiscard work item
func blkdiscardWorkName(volume *v1alpha1.Volume) string {
	return "blkdiscard/" + volume.Spec.VgName + "/" + volume.Name
}

// Populate a Linear volume from any source.
func (r *VolumeNodeReconciler) reconcileLinearPopulating(ctx context.Context, volume *v1alpha1.Volume) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	// Activate so that the node reconciler can run dd and/or blkdiscard.
	_, err := commands.Lvm("lvchange",
		"--devicesfile", volume.Spec.VgName,
		"--activate", "ey",
		volume.Spec.VgName+"/"+volume.Name)
	if err != nil {
		return err
	}

	targetPathOnHost := "/dev/" + volume.Spec.VgName + "/" + volume.Name
	LvmLvKeySafeOffset := config.Domain + "/safe-offset"
	offset, err := commands.LvmLvGetCounter(volume.Spec.VgName, volume.Name, LvmLvKeySafeOffset)
	if err != nil {
		return err
	}
	log.Info("populating linear volume", "offset", offset)

	// Populate the data, but only if it has not yet been populated.
	if volume.Spec.Contents.ContentsType != v1alpha1.VolumeContentsTypeEmpty && offset == 0 {
		log.Info("populating linear volume from source")
		offset, err = r.reconcileSourcePopulating(ctx, volume, targetPathOnHost)
		if err != nil {
			return err
		}

		_, err = commands.LvmLvUpdateCounterIfLower(volume.Spec.VgName, volume.Name, LvmLvKeySafeOffset, offset)
		if err != nil {
			return err
		}
	}

	// Linear volumes contain the previous contents of the disk, which can
	// be an information leak if multiple users have access to the same
	// Volume Group. Zero the LV to avoid security issues.  Filesystems,
	// or an explicit UnsafeFast wipe policy, can skip this wipe.
	//
	// We track how much of the image has been zeroed and then
	// exposed to the user with a counter tag, in order to support
	// image expansion.  We must never touch an offset prior to
	// the stored value, and must never expose the image to the
	// user while the stored value is less than the lv size.
	if volume.DeletionTimestamp != nil {
		log.Info("Canceling blkdiscard work due to volume deletion", "volumeName", volume.Name)
		if err := r.workers.Cancel(blkdiscardWorkName(volume)); err != nil {
			return err
		}
	} else if volume.Spec.Type.Block != nil {
		sizeBytes, err := commands.LvmSize(volume.Spec.VgName, volume.Name)
		if err != nil {
			return err
		}

		if offset < sizeBytes && volume.Spec.WipePolicy != v1alpha1.VolumeWipePolicyUnsafeFast {
			log.Info("populating linear volume with zeroes")
			work := &blkdiscardWork{
				device: targetPathOnHost,
				offset: offset,
			}
			if err := r.workers.Run(blkdiscardWorkName(volume), volume, work); err != nil {
				return err
			}
		}

		_, err = commands.LvmLvUpdateCounterIfLower(volume.Spec.VgName, volume.Name, LvmLvKeySafeOffset, sizeBytes)
		if err != nil {
			return err
		}
	}

	log.Info("done populating linear volume")
	_, err = commands.Lvm("lvchange",
		"--devicesfile", volume.Spec.VgName,
		"--activate", "n",
		volume.Spec.VgName+"/"+volume.Name)
	return err
}

func (r *VolumeNodeReconciler) reconcileThin(ctx context.Context, volume *v1alpha1.Volume) error {
	thinPoolLv := &v1alpha1.ThinPoolLv{}

	if err := r.Get(ctx, types.NamespacedName{Name: volume.Name, Namespace: config.Namespace}, thinPoolLv); err != nil {
		if volume.DeletionTimestamp != nil {
			return client.IgnoreNotFound(err)
		}
		return err
	}

	thinLvName := thinpoollv.VolumeToThinLvName(volume.Name)
	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	if thinLvStatus == nil {
		if volume.DeletionTimestamp != nil && !slices.Contains(volume.Status.AttachedToNodes, config.LocalNodeName) {
			return nil
		}
		return errors.NewBadRequest("unexpected missing blob")
	}
	if err := r.reconcileThinNBDCleanup(ctx, volume, thinPoolLv, thinLvStatus.SizeBytes); err != nil {
		return err
	}

	var attached bool
	var err error
	if volume.DeletionTimestamp == nil && slices.Contains(volume.Spec.AttachToNodes, config.LocalNodeName) {
		err = r.reconcileThinAttaching(ctx, volume, thinPoolLv)
		attached = true
	} else {
		err = r.reconcileThinDetaching(ctx, volume, thinPoolLv)
		attached = false
	}

	if err != nil {
		return err
	}

	if err := r.updateStatusAttachedToNodes(ctx, volume, attached, thinPoolLv); err != nil {
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

	// perform data population

	labels := volume.GetLabels()
	if meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionLvCreated) &&
		!meta.IsStatusConditionTrue(volume.Status.Conditions, v1alpha1.VolumeConditionDataSourceCompleted) &&
		labels != nil && labels[config.PopulationNodeLabel] == config.LocalNodeName {
		var err error

		switch volume.Spec.Mode {
		case v1alpha1.VolumeModeThin:
			err = r.reconcileThinPopulating(ctx, volume)
		case v1alpha1.VolumeModeLinear:
			err = r.reconcileLinearPopulating(ctx, volume)
		default:
			err = errors.NewBadRequest("invalid volume mode for data population")
		}

		if err != nil || volume.DeletionTimestamp != nil {
			if watch, ok := err.(*util.WatchPending); ok {
				log.Info("reconcile waiting for Watch during data population", "why", watch.Why)
				return ctrl.Result{}, nil // wait until Watch triggers
			}
			return ctrl.Result{}, err
		}

		// signal to the cluster controller that data population is done

		condition := metav1.Condition{
			Type:    v1alpha1.VolumeConditionDataSourceCompleted,
			Status:  metav1.ConditionTrue,
			Reason:  "Completed",
			Message: "population from data source completed",
		}
		if meta.SetStatusCondition(&volume.Status.Conditions, condition) {
			if err := r.statusUpdate(ctx, volume); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// check if ready for operation

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

	if watch, ok := err.(*util.WatchPending); ok {
		log.Info("reconcile waiting for Watch", "why", watch.Why)
		return ctrl.Result{}, nil // wait until Watch triggers
	}

	return ctrl.Result{}, err
}

func (r *VolumeNodeReconciler) statusUpdate(ctx context.Context, volume *v1alpha1.Volume) error {
	volume.Status.ObservedGeneration = volume.Generation
	return r.Status().Update(ctx, volume)
}
