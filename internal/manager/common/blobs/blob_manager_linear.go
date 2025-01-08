// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/internal/common/commands"
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

type blkdiscardWork struct {
	vgName string
	lvName string
}

func (w *blkdiscardWork) Run(ctx context.Context) error {
	log := log.FromContext(ctx)
	return commands.WithLvmLvActivated(w.vgName, w.lvName, func() error {
		path := fmt.Sprintf("/dev/%s/%s", w.vgName, w.lvName)
		log.Info("blkdiscard worker zeroing LV", "path", path)
		_, err := commands.RunOnHostContext(ctx, "blkdiscard", "--zeroout", path)
		// To test long-running operations: _, err := commands.RunOnHostContext(ctx, "sleep", "30")
		log.Info("blkdiscard worker finished", "path", path)
		return err
	})
}

// Returns a unique name for a blkdiscard work item
func (m *LinearBlobManager) blkdiscardWorkName(name string) string {
	return fmt.Sprintf("blkdiscard/%s/%s", m.vgName, name)
}

func (m *LinearBlobManager) CreateBlob(ctx context.Context, name string, sizeBytes int64, owner client.Object) error {
	_, err := commands.LvmLvCreateIdempotent(
		"--devicesfile", m.vgName,
		"--activate", "n",
		"--type", "linear",
		"--metadataprofile", "kubesan",
		"--name", name,
		"--size", fmt.Sprintf("%db", sizeBytes),
		m.vgName,
	)
	if err != nil {
		return err
	}

	// TODO recreate if size does not match. This handles the case where a
	// blob was partially created and then reconciled again with a
	// different size. A blob must never be recreated after volume creation
	// has completed since that could lose data!

	// Linear volumes contain the previous contents of the disk, which can
	// be an information leak if multiple users have access to the same
	// Volume Group. Zero the LV to avoid security issues.
	LvmLvTagZeroed := "kubesan.gitlab.io/zeroed=true"
	hasTag, err := commands.LvmLvHasTag(m.vgName, name, LvmLvTagZeroed)
	if err != nil {
		return err
	}
	if !hasTag {
		work := &blkdiscardWork{
			vgName: m.vgName,
			lvName: name,
		}
		err := m.workers.Run(m.blkdiscardWorkName(name), owner, work)
		if err != nil {
			return err
		}

		err = commands.LvmLvAddTag(m.vgName, name, LvmLvTagZeroed)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *LinearBlobManager) RemoveBlob(ctx context.Context, name string, owner client.Object) error {
	// stop blkdiscard in case it's running
	if err := m.workers.Cancel(m.blkdiscardWorkName(name)); err != nil {
		return err
	}

	_, err := commands.LvmLvRemoveIdempotent(
		"--devicesfile", m.vgName,
		fmt.Sprintf("%s/%s", m.vgName, name),
	)
	return err
}

func (m *LinearBlobManager) GetPath(name string) string {
	return fmt.Sprintf("/dev/%s/%s", m.vgName, name)
}
