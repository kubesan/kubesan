// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
)

type ThinPoolLvNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func SetUpThinPoolLvNodeReconciler(mgr ctrl.Manager) error {
	r := &ThinPoolLvNodeReconciler{
		Client: client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		Scheme: mgr.GetScheme(),
	}

	// KubeSAN VGs use their own LVM profile to avoid interfering with the
	// system-wide lvm.conf configuration. This profile is hardcoded here and is
	// put in place before creating LVs that get their configuration from the
	// profile.

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		For(&v1alpha1.ThinPoolLv{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=thinpoollvs,verbs=get;list;watch;create;update;patch;delete,namespace=kubesan-system
// +kubebuilder:rbac:groups=kubesan.gitlab.io,resources=thinpoollvs/status,verbs=get;update;patch,namespace=kubesan-system

func (r *ThinPoolLvNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	log.Info("ThinPoolLvNodeReconciler entered")
	defer log.Info("ThinPoolLvNodeReconciler exited")

	thinPoolLv := &v1alpha1.ThinPoolLv{}
	if err := r.Get(ctx, req.NamespacedName, thinPoolLv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// only run after the cluster controller has created the thin-pool

	if !meta.IsStatusConditionTrue(thinPoolLv.Status.Conditions, v1alpha1.ThinPoolLvConditionAvailable) {
		return ctrl.Result{}, nil
	}

	// TODO rebuild Status from on-disk thin-pool state to achieve fault tolerance (e.g. etcd out of sync with disk)

	log.Info("ThinPoolLv is activated, proceeding with node reconcile()")

	stayActive, err := r.reconcileThinPoolLvActivation(ctx, thinPoolLv)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Thin LV commands like lvcreate(8) and lvremove(8) leave the
	// thin-pool activated. Make sure to deactivate the thin-pool before
	// returning, unless a thin LV has been activated.

	if !stayActive {
		defer func() {
			_, _ = commands.Lvm(
				"lvchange",
				"--devicesfile", thinPoolLv.Spec.VgName,
				"--activate", "n",
				fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinPoolLv.Name),
			)
		}()
	}

	// only continue when this node is the active node and we're not undergoing deletion

	if (thinPoolLv.DeletionTimestamp != nil && len(thinPoolLv.Status.ThinLvs) == 0) || thinPoolLv.Spec.ActiveOnNode != config.LocalNodeName {
		log.Info("ThinPoolLv is not active on this node or is being deleted")
		return ctrl.Result{}, nil
	}

	log.Info("ThinPoolLv is active on this node, proceeding")

	if thinPoolLv.DeletionTimestamp == nil {
		err = r.reconcileThinLvCreation(ctx, thinPoolLv)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	err = r.reconcileThinLvActivations(ctx, thinPoolLv)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.reconcileThinLvDeletion(ctx, thinPoolLv)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// Returns true if the thin-pool should be active
func (r *ThinPoolLvNodeReconciler) reconcileThinPoolLvActivation(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) (bool, error) {
	thinPoolLvShouldBeActive :=
		kubesanslices.Any(thinPoolLv.Spec.ThinLvs, func(spec v1alpha1.ThinLvSpec) bool { return spec.State.Name == v1alpha1.ThinLvSpecStateNameActive })

	if thinPoolLvShouldBeActive {
		if thinPoolLv.Spec.ActiveOnNode == config.LocalNodeName {
			// activate LVM thin pool LV

			_, err := commands.Lvm(
				"lvchange",
				"--devicesfile", thinPoolLv.Spec.VgName,
				"--activate", "ey",
				"--monitor", "y",
				fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinPoolLv.Name),
			)
			if err != nil {
				return thinPoolLvShouldBeActive, err
			}

			condition := metav1.Condition{
				Type:    v1alpha1.ThinPoolLvConditionActive,
				Status:  metav1.ConditionTrue,
				Reason:  "Activated",
				Message: "thin pool activated",
			}
			meta.SetStatusCondition(&thinPoolLv.Status.Conditions, condition)

			thinPoolLv.Status.ActiveOnNode = config.LocalNodeName

			if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
				return thinPoolLvShouldBeActive, err
			}
		}
	} else {
		if thinPoolLv.Status.ActiveOnNode == config.LocalNodeName {
			// Deactivate all LVM thin LVs. The `vgchange
			// --activate n --force` flag could be used on the
			// thin-pool LV instead of deactivating thin LVs
			// individually. However, the `--force` flag would hide
			// issues like thin LVs going out of sync with
			// Status.ThinLvs[] so fail noisily to aid debugging.

			for i := range thinPoolLv.Status.ThinLvs {
				thinLvStatus := &thinPoolLv.Status.ThinLvs[i]

				output, err := commands.Lvm(
					"lvchange",
					"--devicesfile", thinPoolLv.Spec.VgName,
					"--activate", "n",
					fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinLvStatus.Name),
				)
				if err != nil {
					if strings.Contains(string(output.Combined), "ailed to find") {
						// Ignore error if lv is already gone
					} else {
						return thinPoolLvShouldBeActive, err
					}
				}

				thinLvStatus.State = v1alpha1.ThinLvStatusState{
					Name: v1alpha1.ThinLvStatusStateNameInactive,
				}
			}

			// deactivate LVM thin pool LV

			_, err := commands.Lvm(
				"lvchange",
				"--devicesfile", thinPoolLv.Spec.VgName,
				"--activate", "n",
				fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinPoolLv.Name),
			)
			if err != nil {
				return thinPoolLvShouldBeActive, err
			}

			condition := metav1.Condition{
				Type:    v1alpha1.ThinPoolLvConditionActive,
				Status:  metav1.ConditionFalse,
				Reason:  "Deactivated",
				Message: "thin pool deactivated",
			}
			meta.SetStatusCondition(&thinPoolLv.Status.Conditions, condition)

			thinPoolLv.Status.ActiveOnNode = ""

			if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
				return thinPoolLvShouldBeActive, err
			}
		}
	}

	return thinPoolLvShouldBeActive, nil
}

func (r *ThinPoolLvNodeReconciler) reconcileThinLvDeletion(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	needUpdate := false
	log := log.FromContext(ctx)

	// remove thin LVs from thin-pool that have been marked for removal in Spec.ThinLvs[]

	for i := 0; i < len(thinPoolLv.Spec.ThinLvs); i++ {
		thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
		if thinLvSpec.State.Name != v1alpha1.ThinLvSpecStateNameRemoved {
			continue
		}

		log.Info("Deleting", "thin LV", thinLvSpec.Name)

		err := r.removeThinLv(ctx, thinPoolLv, thinLvSpec.Name)
		if err != nil {
			return err
		}

		needUpdate = true
	}

	if !needUpdate {
		return nil
	}

	// drop thin LVs from Status.ThinLvs[] that were removed by Spec.ThinLvs[]

	newThinLvs := make([]v1alpha1.ThinLvStatus, 0, len(thinPoolLv.Spec.ThinLvs))
	for i := 0; i < len(thinPoolLv.Status.ThinLvs); i++ {
		thinLvStatus := &thinPoolLv.Status.ThinLvs[i]
		thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvStatus.Name)
		if thinLvSpec == nil {
			err := errors.NewBadRequest(fmt.Sprintf("spec for volume \"%s\" went missing without requesting state Removed", thinLvStatus.Name))
			log.Error(err, "refusing to remove ThinLv")
			return err
		} else if thinLvSpec.State.Name != v1alpha1.ThinLvSpecStateNameRemoved {
			newThinLvs = append(newThinLvs, *thinLvStatus)
		}
	}
	thinPoolLv.Status.ThinLvs = newThinLvs

	return r.statusUpdate(ctx, thinPoolLv)
}

// Handles both LV creation and expansion
func (r *ThinPoolLvNodeReconciler) reconcileThinLvCreation(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	log := log.FromContext(ctx)
	for i := range thinPoolLv.Spec.ThinLvs {
		thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
		thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvSpec.Name)

		if thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameRemoved {
			// nothing to do here
		} else if thinLvStatus == nil {
			log.Info("Creating", "thin LV", thinLvSpec.Name)

			err := r.createThinLv(ctx, thinPoolLv, thinLvSpec)
			if err != nil {
				return err
			}
		} else if thinLvSpec.SizeBytes > thinLvStatus.SizeBytes && !thinLvSpec.ReadOnly {
			log.Info("Expanding", "thin LV", thinLvSpec.Name)

			err := r.expandThinLv(ctx, thinPoolLv, thinLvSpec, thinLvStatus)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *ThinPoolLvNodeReconciler) reconcileThinLvActivations(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	// Update thin LV activations to match spec

	for i := range thinPoolLv.Status.ThinLvs {
		thinLvStatus := &thinPoolLv.Status.ThinLvs[i]
		thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvStatus.Name)

		shouldBeActive := thinLvSpec != nil && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive
		isActiveInStatus := thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameActive

		path := "/dev/" + thinPoolLv.Spec.VgName + "/" + thinLvStatus.Name
		isActuallyActive, err := commands.PathExistsOnHost(path)
		if err != nil {
			return err
		}

		if shouldBeActive {
			if !isActuallyActive {
				// activate LVM thin LV

				_, err = commands.Lvm(
					"lvchange",
					"--devicesfile", thinPoolLv.Spec.VgName,
					"--activate", "ey",
					thinPoolLv.Spec.VgName+"/"+thinLvStatus.Name,
				)
				if err != nil {
					return err
				}
			}

			// update status to reflect reality if necessary

			if !isActiveInStatus {
				thinLvStatus.State = v1alpha1.ThinLvStatusState{
					Name: v1alpha1.ThinLvStatusStateNameActive,
					Active: &v1alpha1.ThinLvStatusStateActive{
						Path: path,
					},
				}

				if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
					return err
				}
			}
		} else {
			if isActuallyActive {
				// deactivate LVM thin LV

				_, err = commands.Lvm(
					"lvchange",
					"--devicesfile", thinPoolLv.Spec.VgName,
					"--activate", "n",
					thinPoolLv.Spec.VgName+"/"+thinLvStatus.Name,
				)
				if err != nil {
					return err
				}
			}

			// update status to reflect reality if necessary

			if isActiveInStatus {
				thinLvStatus.State = v1alpha1.ThinLvStatusState{
					Name: v1alpha1.ThinLvStatusStateNameInactive,
				}

				if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (r *ThinPoolLvNodeReconciler) createThinLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv, thinLvSpec *v1alpha1.ThinLvSpec) error {
	log := log.FromContext(ctx)

	switch thinLvSpec.Contents.ContentsType {
	case v1alpha1.ThinLvContentsTypeEmpty:
		// create empty LVM thin LV
		log.Info("Creating an empty thin LV")

		_, err := commands.LvmLvCreateIdempotent(
			thinLvSpec.Binding,
			"--devicesfile", thinPoolLv.Spec.VgName,
			"--type", "thin",
			"--name", thinLvSpec.Name,
			"--thinpool", thinPoolLv.Name,
			"--virtualsize", fmt.Sprintf("%db", thinLvSpec.SizeBytes),
			thinPoolLv.Spec.VgName,
		)
		if err != nil {
			return err
		}

		// deactivate LVM thin LV (`--activate n` has no effect on `lvcreate --type thin`)

		_, err = commands.Lvm(
			"lvchange",
			"--devicesfile", thinPoolLv.Spec.VgName,
			"--activate", "n",
			fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinLvSpec.Name),
		)
		if err != nil {
			return err
		}

	case v1alpha1.ThinLvContentsTypeSnapshot:
		if thinLvSpec.Contents.Snapshot == nil {
			log.Info("Missing snapshot contents", "thin LV", thinLvSpec.Name)
			return nil
		}

		log.Info("Creating a snapshot LV")

		sourceLv := thinLvSpec.Contents.Snapshot.SourceThinLvName

		if thinPoolLv.Status.FindThinLv(sourceLv) == nil {
			// source thin LV does not (currently) exist
			return nil
		}

		// create snapshot LVM thin LV

		_, err := commands.LvmLvCreateIdempotent(
			thinLvSpec.Binding,
			"--devicesfile", thinPoolLv.Spec.VgName,
			"--name", thinLvSpec.Name,
			"--snapshot",
			"--setactivationskip", "n",
			"--permission", "r",
			fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, sourceLv),
		)
		if err != nil {
			return err
		}

	default:
		// unknown LVM thin LV contents
		log.Info("Unknown contents", "thin LV", thinLvSpec.Name)
		return nil
	}

	size, err := commands.LvmSize(thinPoolLv.Spec.VgName, thinLvSpec.Name)
	if err != nil {
		return err
	}

	thinLvStatus := v1alpha1.ThinLvStatus{
		Name: thinLvSpec.Name,
		State: v1alpha1.ThinLvStatusState{
			Name: v1alpha1.ThinLvStatusStateNameInactive,
		},
		SizeBytes: size,
	}

	thinPoolLv.Status.ThinLvs = append(thinPoolLv.Status.ThinLvs, thinLvStatus)

	if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
		return err
	}

	log.Info("Successfully created", "thin LV", thinLvSpec.Name)

	return nil
}

func (r *ThinPoolLvNodeReconciler) removeThinLv(_ context.Context, thinPoolLv *v1alpha1.ThinPoolLv, thinLvName string) error {
	_, err := commands.LvmLvRemoveIdempotent(
		"--devicesfile", thinPoolLv.Spec.VgName,
		fmt.Sprintf("%s/%s", thinPoolLv.Spec.VgName, thinLvName),
	)
	return err
}

func (r *ThinPoolLvNodeReconciler) expandThinLv(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv, thinLvSpec *v1alpha1.ThinLvSpec, thinLvStatus *v1alpha1.ThinLvStatus) error {
	log := log.FromContext(ctx)

	_, err := commands.LvmLvExtendIdempotent(
		"--devicesfile", thinPoolLv.Spec.VgName,
		"--size", fmt.Sprintf("%db", thinLvSpec.SizeBytes),
		thinPoolLv.Spec.VgName+"/"+thinLvSpec.Name,
	)
	if err != nil {
		return err
	}

	size, err := commands.LvmSize(thinPoolLv.Spec.VgName, thinLvSpec.Name)
	if err != nil {
		return err
	}

	thinLvStatus.SizeBytes = size
	if err := r.statusUpdate(ctx, thinPoolLv); err != nil {
		return err
	}

	log.Info("Successfully expanded", "thin LV", thinLvSpec.Name)

	return nil
}

func (r *ThinPoolLvNodeReconciler) statusUpdate(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv) error {
	thinPoolLv.Status.ObservedGeneration = thinPoolLv.Generation
	return r.Status().Update(ctx, thinPoolLv)
}
