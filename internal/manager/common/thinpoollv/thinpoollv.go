// SPDX-License-Identifier: Apache-2.0
//
// Common ThinPoolLv code used by cluster and node controllers

package thinpoollv

import (
	"context"
	"reflect"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Returns the thin LV name given the volume's name. This is necessary because
// thin-pool LV and thin LV names are in the same namespace and must be unique.
func VolumeToThinLvName(volumeName string) string {
	return "thin-" + volumeName
}

// Maps from ThinLvSpecState.Name to ThinLvStatusState.Name
func SpecStateToStatusState(specStateName string) string {
	switch specStateName {
	case v1alpha1.ThinLvSpecStateNameInactive:
		return v1alpha1.ThinLvStatusStateNameInactive
	case v1alpha1.ThinLvSpecStateNameActive:
		return v1alpha1.ThinLvStatusStateNameActive
	case v1alpha1.ThinLvSpecStateNameRemoved:
		return v1alpha1.ThinLvStatusStateNameRemoved
	default:
		return ""
	}
}

// Returns true if the ThinPoolLv should be active on a node
func thinPoolLvNeedsActivation(thinPoolLv *v1alpha1.ThinPoolLv) bool {
	// Cases that have been considered:
	// 1. Thin LV creation
	// 2. Thin LV deletion
	// 3. Thin LV activation
	// 4. Thin LV extension
	//
	// Update this list when you change which cases are handled by this
	// function. That way it will be easier to identify what still needs to
	// be implemented. It is critical that this function detects all cases
	// requiring activation/deactivation, otherwise the thin-pool will not
	// be activated appropriately or may remain activated when it shouldn't
	// be!
	// TODO populating from contents (cloning)

	for i := range thinPoolLv.Spec.ThinLvs {
		thinLvSpec := &thinPoolLv.Spec.ThinLvs[i]
		thinLvStatus := thinPoolLv.Status.FindThinLv(thinLvSpec.Name)

		// corresponding Status.ThinLvs[] element doesn't exist yet

		if thinLvStatus == nil {
			return true
		}

		// thin LV is undergoing a state transition (e.g. creation/deletion/activation/deactivation)

		if SpecStateToStatusState(thinLvSpec.State.Name) != thinLvStatus.State.Name {
			return true
		}

		// thin LV is explicitly activated

		if thinLvSpec.State.Name == v1alpha1.ThinLvSpecStateNameActive {
			return true
		}

		// extending the thin LV requires that the ThinPoolLv be active on a node

		if thinLvSpec.SizeBytes > thinLvStatus.SizeBytes {
			return true
		}
	}

	for i := range thinPoolLv.Status.ThinLvs {
		thinLvStatus := &thinPoolLv.Status.ThinLvs[i]

		// Spec.ThinLvs[] element has been removed but corresponding
		// Status.ThinLvs[] hasn't been cleaned up by ThinPoolLv node
		// controller yet

		if thinLvStatus.State.Name == v1alpha1.ThinLvStatusStateNameRemoved &&
			thinPoolLv.Spec.FindThinLv(thinLvStatus.Name) == nil {
			return true
		}
	}

	return false
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
func recalcActiveOnNode(thinPoolLv *v1alpha1.ThinPoolLv) {
	needsActivation := thinPoolLvNeedsActivation(thinPoolLv)

	if needsActivation && thinPoolLv.Spec.ActiveOnNode == "" {
		// TODO replace with better node selection policy?
		thinPoolLv.Spec.ActiveOnNode = config.LocalNodeName
	}

	if !needsActivation && thinPoolLv.Spec.ActiveOnNode != "" {
		thinPoolLv.Spec.ActiveOnNode = ""
	}

	if thinPoolLv.DeletionTimestamp != nil {
		// Once deletion is requested, the currently-active
		// node (if any) will automatically deactivate, and
		// then the cluster reconciler will take over.
		thinPoolLv.Spec.ActiveOnNode = thinPoolLv.Status.ActiveOnNode
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

	recalcActiveOnNode(thinPoolLv)
	if reflect.DeepEqual(oldThinPoolLv, thinPoolLv) {
		log.Info("ThinPoolLv update not needed", "Spec.ActiveOnNode", thinPoolLv.Spec.ActiveOnNode, "Status.ActiveOnNode", thinPoolLv.Status.ActiveOnNode)
		return nil
	}
	log.Info("Updating ThinPoolLv", "former", former, "preferred", preferred, "Spec.ActiveOnNode", thinPoolLv.Spec.ActiveOnNode, "Status.ActiveOnNode", thinPoolLv.Status.ActiveOnNode)
	return client.Update(ctx, thinPoolLv)
}
