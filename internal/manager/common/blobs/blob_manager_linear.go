// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

type blkdiscardWork struct {
	vgName   string
	lvName   string
	offset   int64
	skipWipe bool
}

func (w *blkdiscardWork) Run(ctx context.Context) error {
	log := log.FromContext(ctx)
	if w.skipWipe && w.offset > 0 {
		return nil
	}
	return commands.WithLvmLvActivated(w.vgName, w.lvName, true, func() error {
		path := fmt.Sprintf("/dev/%s/%s", w.vgName, w.lvName)
		args := []string{"blkdiscard", "--zeroout", "--offset",
			strconv.FormatInt(w.offset, 10)}
		if w.skipWipe {
			args = append(args, "--length", strconv.Itoa(4*1024*1024))
		}
		args = append(args, path)
		log.Info("blkdiscard worker zeroing LV", "path", path, "command", args)
		_, err := commands.RunOnHostContext(ctx, args...)
		// To test long-running operations: _, err := commands.RunOnHostContext(ctx, "sleep", "30")
		log.Info("blkdiscard worker finished", "path", path)
		return err
	})
}

// Returns a unique name for a blkdiscard work item
func (m *LinearBlobManager) blkdiscardWorkName(name string) string {
	return fmt.Sprintf("blkdiscard/%s/%s", m.vgName, name)
}

// Create or expand a blob.
func (m *LinearBlobManager) CreateBlob(ctx context.Context, name string, sizeBytes int64, skipWipe bool, owner client.Object) error {
	// This creates but does not resize.
	_, err := commands.LvmLvCreateIdempotent(
		"",
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

	// Linear volumes contain the previous contents of the disk, which can
	// be an information leak if multiple users have access to the same
	// Volume Group. Zero the LV to avoid security issues.
	//
	// We track how much of the image has been zeroed and then
	// exposed to the user with a counter tag, in order to support
	// image expansion.  We must never touch an offset prior to
	// the stored value, and must never expose the image to the
	// user while the stored value is less than the lv size.
	LvmLvKeySafeOffset := config.Domain + "/safe-offset"
	offset, err := commands.LvmLvGetCounter(m.vgName, name, LvmLvKeySafeOffset)
	if err != nil {
		return err
	}

	if offset < sizeBytes {
		if _, err := commands.LvmLvExtendIdempotent(
			"--devicesfile", m.vgName,
			"--size", fmt.Sprintf("%db", sizeBytes),
			m.vgName+"/"+name,
		); err != nil {
			return err
		}

		work := &blkdiscardWork{
			vgName:   m.vgName,
			lvName:   name,
			offset:   offset,
			skipWipe: skipWipe,
		}
		err := m.workers.Run(m.blkdiscardWorkName(name), owner, work)
		if err != nil {
			return err
		}

		_, err = commands.LvmLvUpdateCounterIfLower(m.vgName, name, LvmLvKeySafeOffset, sizeBytes)
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

func (m *LinearBlobManager) SnapshotBlob(ctx context.Context, name string, sourceName string, owner client.Object) error {
	// Linear volumes do not support snapshots
	return errors.NewBadRequest("linear volumes do not support snapshots")
}

func (m *LinearBlobManager) ActivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	// Linear volumes do not support cloning
	return errors.NewBadRequest("linear volumes do not support cloning")
}

func (m *LinearBlobManager) ActivateBlobForCloneTarget(ctx context.Context, name string, dataSrcBlobMgr BlobManager) error {
	// Linear volumes do not support cloning
	return errors.NewBadRequest("linear volumes do not support cloning")
}

func (m *LinearBlobManager) DeactivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error {
	// Linear volumes do not support cloning
	return errors.NewBadRequest("linear volumes do not support cloning")
}

func (m *LinearBlobManager) DeactivateBlobForCloneTarget(ctx context.Context, name string) error {
	// Linear volumes do not support cloning
	return errors.NewBadRequest("linear volumes do not support cloning")
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
