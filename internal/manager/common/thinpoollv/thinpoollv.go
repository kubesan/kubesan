// SPDX-License-Identifier: Apache-2.0
//
// Common ThinPoolLv code used by cluster and node controllers

package thinpoollv

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
)

// Returns the thin LV name given the volume's name. This is necessary because
// thin-pool LV and thin LV names are in the same namespace and must be unique.
func VolumeToThinLvName(volumeName string) string {
	return "thin-" + volumeName
}

// Maps from ThinLvSpecState.Name to ThinLvStatusState.Name
func specStateToStatusState(specStateName string) string {
	switch specStateName {
	case v1alpha1.ThinLvSpecStateNameInactive:
		return v1alpha1.ThinLvStatusStateNameInactive
	case v1alpha1.ThinLvSpecStateNameActive:
		return v1alpha1.ThinLvStatusStateNameActive
	default:
		return ""
	}
}

// Returns true if the ThinPoolLv should be active on a node.
// It is an error to request a new ThinLv if the pool is marked for deletion.
func thinPoolLvNeedsActivation(thinPoolLv *v1alpha1.ThinPoolLv) (bool, error) {
	// Cases that have been considered:
	// 1. Thin LV creation
	// 2. Thin LV deletion
	// 3. Thin LV activation
	// 4. Thin LV extension
	// 5. Thin pool marked for deletion
	//
	// Update this list when you change which cases are handled by this
	// function. That way it will be easier to identify what still needs to
	// be implemented. It is critical that this function detects all cases
	// requiring activation/deactivation, otherwise the thin-pool will not
	// be activated appropriately or may remain activated when it shouldn't
	// be!

	for i := range thinPoolLv.Spec.ThinLvs {
		thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
		thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvSpec.Name)

		// No corresponding Status.ThinLvs[] element.  Either this is
		// creation (activation needed), or we just succeeded at
		// removal (caller will compress the array later).

		if thinLvStatus == nil {
			if thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameRemoved {
				continue
			}
			if thinPoolLv.DeletionTimestamp != nil {
				return false, errors.NewBadRequest("cannot create a new ThinLV in a pool marked for deletion")
			}
			return true, nil
		}

		// thin LV is undergoing a state transition (e.g. creation/deletion/activation/deactivation)

		if specStateToStatusState(thinLvSpec.State.Name) != thinLvStatus.State.Name {
			return true, nil
		}

		// thin LV is explicitly activated

		if thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
			return true, nil
		}

		// extending the thin LV requires that the ThinPoolLv be active on a node

		if thinLvSpec.SizeBytes > thinLvStatus.SizeBytes && !thinLvSpec.ReadOnly {
			return true, nil
		}
	}

	return false, nil
}

// Check whether work is pending that needs thin-pool activation. If yes,
// assign a suitable node to thinPoolLv.Spec.ActiveOnNode (if it is not already
// assigned). If no, clear thinPoolLv.Spec.ActiveOnNode.  Furthermore, if
// thinPoolLv.Spec.ActiveOnNode wants to activate on a different node than
// where thinPoolLv.Status.ActiveOnNode is already active, this rewrites
// to a deactivation request on thinPoolLv.Status.ActiveOnNode.
//
// This is a level-triggered operation. It must be called every time we start
// or stop using the ThinPoolLv because its controller does not process updates
// when thinPoolLv.Spec.ActiveOnNode is clear. The thin-pool must also be
// deactivated once work has finished so it doesn't stay activated on a node
// where they are not needed.
func recalcActiveOnNode(thinPoolLv *v1alpha1.ThinPoolLv) error {
	needsActivation, err := thinPoolLvNeedsActivation(thinPoolLv)
	if err != nil {
		return err
	}

	if needsActivation && thinPoolLv.Spec.ActiveOnNode == "" {
		// TODO replace with better node selection policy?
		thinPoolLv.Spec.ActiveOnNode = config.LocalNodeName
	}

	if !needsActivation && thinPoolLv.Spec.ActiveOnNode != "" {
		thinPoolLv.Spec.ActiveOnNode = ""
	}

	if thinPoolLv.Status.ActiveOnNode != "" && thinPoolLv.Spec.ActiveOnNode != thinPoolLv.Status.ActiveOnNode {
		// In order to switch Spec.ActiveOnNode, we must first tell
		// the currently-active node to deactivate.
		thinPoolLv.Spec.ActiveOnNode = thinPoolLv.Status.ActiveOnNode
		for i := range thinPoolLv.Spec.ThinLvs {
			thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
			if thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
				thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameInactive
			}
		}
	}
	return nil
}

// Recalculate Spec.ActiveOnNode and invoke client.Update() for thinPoolLv
// if it has changed from oldThinPoolLv.
func UpdateThinPoolLv(ctx context.Context, client client.Client, oldThinPoolLv, thinPoolLv *v1alpha1.ThinPoolLv) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)
	preferred := thinPoolLv.Spec.ActiveOnNode
	former := "?"
	if oldThinPoolLv != nil {
		former = oldThinPoolLv.Spec.ActiveOnNode
	}

	if err := recalcActiveOnNode(thinPoolLv); err != nil {
		return err
	}
	if reflect.DeepEqual(oldThinPoolLv, thinPoolLv) {
		log.Info("ThinPoolLv update not needed", "Spec.ActiveOnNode", thinPoolLv.Spec.ActiveOnNode, "Status.ActiveOnNode", thinPoolLv.Status.ActiveOnNode)
		return nil
	}
	log.Info("Updating ThinPoolLv", "former", former, "preferred", preferred, "Spec.ActiveOnNode", thinPoolLv.Spec.ActiveOnNode, "Status.ActiveOnNode", thinPoolLv.Status.ActiveOnNode)
	err := client.Update(ctx, thinPoolLv)
	if err == nil && oldThinPoolLv != nil {
		thinPoolLv.DeepCopyInto(oldThinPoolLv)
	}
	return err
}

func ActivateThinLv(ctx context.Context, client client.Client, oldThinPoolLv, thinPoolLv *v1alpha1.ThinPoolLv, name string) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	thinLvName := VolumeToThinLvName(name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	if thinLvSpec == nil {
		return errors.NewBadRequest(fmt.Sprintf("thinLv \"%s\" not found", thinLvName))
	}

	switch thinLvSpec.State.Name {
	case v1alpha1.ThinLvSpecStateNameInactive:
		log.Info("Setting thin LV state to active")
		thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameActive
	case v1alpha1.ThinLvSpecStateNameActive: // do nothing
	default:
		return errors.NewBadRequest(fmt.Sprintf("unexpected state for thinLv \"%s\"", thinLvName))
	}

	if err := UpdateThinPoolLv(ctx, client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}

	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	if thinLvStatus == nil || thinLvStatus.State.Name != v1alpha1.ThinLvStatusStateNameActive {
		return util.NewWatchPending("waiting for ThinLv activation")
	}
	return nil
}

func DeactivateThinLv(ctx context.Context, client client.Client, oldThinPoolLv, thinPoolLv *v1alpha1.ThinPoolLv, name string) error {
	thinLvName := VolumeToThinLvName(name)
	thinLvSpec := thinPoolLv.Spec.FindThinLv(thinLvName)
	if thinLvSpec == nil {
		return errors.NewBadRequest(fmt.Sprintf("thinLv \"%s\" not found", thinLvName))
	}

	switch thinLvSpec.State.Name {
	case v1alpha1.ThinLvSpecStateNameInactive: // do nothing
	case v1alpha1.ThinLvSpecStateNameActive:
		thinLvSpec.State.Name = v1alpha1.ThinLvSpecStateNameInactive
	case v1alpha1.ThinLvSpecStateNameRemoved: // do nothing
	default:
		return errors.NewBadRequest(fmt.Sprintf("unexpected state for thinLv \"%s\"", thinLvName))
	}

	// activation may have changed as a result of the actions above

	if err := UpdateThinPoolLv(ctx, client, oldThinPoolLv, thinPoolLv); err != nil {
		return err
	}

	thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvName)
	if thinLvStatus != nil && thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameActive {
		return util.NewWatchPending("waiting for ThinLv deactivation")
	}
	return nil
}

// Return the list of interested nodes, given the list of staged nodes.
// Order matters; the ThinPoolLv plans to activate the first returned node.
func InterestedNodes(ctx context.Context, thinPoolLv *v1alpha1.ThinPoolLv, attachList []string) []string {
	// If the pool has any temporary snapshots active as a clone
	// source, then the node where they are activated MUST remain
	// active until the cloning is done.
	if thinPoolLv.Spec.ActiveOnNode != "" {
		for i := range thinPoolLv.Spec.ThinLvs {
			thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
			if strings.Contains(thinLvSpec.Name, "clone-") && thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
				return append([]string{thinPoolLv.Spec.ActiveOnNode}, slices.DeleteFunc(attachList, func(node string) bool {
					return node == thinPoolLv.Spec.ActiveOnNode
				})...)
			}
		}
	}
	return attachList
}
