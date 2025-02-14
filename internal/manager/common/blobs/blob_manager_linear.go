// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	"gitlab.com/kubesan/kubesan/internal/manager/common/workers"
)

// Per-reconcile invocation state
type LinearBlobManager struct {
	workers *workers.Workers
	vgName  string
}

// NewLinearBlobManager returns a BlobManager implemented using LVM's linear
// logical volumes. Linear LVs are fully provisioned and support direct
// ReadWriteMany without NBD when used without LVM's COW snapshots. They are a
// natural fit for use cases that require constant RWX and do not need
// snapshots.
func NewLinearBlobManager(workers *workers.Workers, vgName string) BlobManager {
	return &LinearBlobManager{
		workers: workers,
		vgName:  vgName,
	}
}

// Create or expand a blob.
func (m *LinearBlobManager) CreateBlob(ctx context.Context, name string, binding string, sizeBytes int64, skipWipe bool, owner client.Object) error {
	// This creates but does not resize; we use lvm to zero the
	// first 4k, but we can leave the device activated because it
	// will be deactivated when the node reconciler finishes
	// wiping the rest of the volume.
	_, err := commands.LvmLvCreateIdempotent(
		binding,
		"--devicesfile", m.vgName,
		"--activate", "ey",
		"--zero", "y",
		"--wipesignatures", "n", // wipe without asking
		"--type", "linear",
		"--metadataprofile", "kubesan",
		"--name", name,
		"--size", fmt.Sprintf("%db", sizeBytes),
		m.vgName,
	)
	if err != nil {
		return err
	}

	_, err = commands.LvmLvExtendIdempotent(
		"--devicesfile", m.vgName,
		"--size", fmt.Sprintf("%db", sizeBytes),
		m.vgName+"/"+name,
	)
	return err
}

func (m *LinearBlobManager) RemoveBlob(ctx context.Context, name string, owner client.Object) error {
	_, err := commands.LvmLvRemoveIdempotent(
		"--devicesfile", m.vgName,
		fmt.Sprintf("%s/%s", m.vgName, name),
	)
	return err
}

func (m *LinearBlobManager) SnapshotBlob(ctx context.Context, name string, binding string, sourceName string, owner client.Object) error {
	// Linear volumes do not support snapshots
	return errors.NewBadRequest("linear volumes do not support snapshots")
}

func (m *LinearBlobManager) ActivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	// Linear volumes do not support clone source
	return errors.NewBadRequest("linear volumes do not support cloning")
}

func (m *LinearBlobManager) ActivateBlobForCloneTarget(ctx context.Context, name string, dataSrcBlobMgr BlobManager) (string, error) {
	// TODO Linear volumes could also support being a clone destination from a thin snapshot
	return config.LocalNodeName, nil
}

func (m *LinearBlobManager) DeactivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	// Linear volumes do not support clone source
	return errors.NewBadRequest("linear volumes do not support cloning")
}

func (m *LinearBlobManager) DeactivateBlobForCloneTarget(ctx context.Context, name string) error {
	// Deactivation was taken care of in the node reconciler.
	return nil
}

// Return the actual size of the blob.
func (m *LinearBlobManager) GetSize(ctx context.Context, name string) (int64, error) {
	return commands.LvmSize(m.vgName, name)
}

func (m *LinearBlobManager) GetPath(name string) string {
	return fmt.Sprintf("/dev/%s/%s", m.vgName, name)
}

// Linear blobs must be expanded offline.
func (m *LinearBlobManager) ExpansionMustBeOffline() bool {
	return true
}

// Since expansion was offline, only the cluster controller can update size.
func (m *LinearBlobManager) SizeNeedsCheck(staged bool) bool {
	return true
}
