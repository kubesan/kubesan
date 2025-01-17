// SPDX-License-Identifier: Apache-2.0

package dm

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

// This package provides the code for idempotent manipulation of
// device mapper wrappers for thin volumes using dmsetup in the host
// namespace.  Note that "dmsetup suspend/dmsetup resume" is the only
// way to live-swap which underlying block device is dereferenced,
// however, a live-swap does not close the old fd until a resume.
// During migration, we have to have the LV fd closed before we can
// deactivate the LV, and we don't have the new NBD fd to open until
// another node has activated the LV, so we have to resume on
// something else, such as the zero or error table.  But exposing the
// zero or error table to the end user would break their I/O.
//
// The solution is to use two linear devices per Volume.  On a suspend
// request, the upper layer starts a long-running suspend, then the
// lower layer can do a suspend and resume into the zero device;
// because the upper layer is still suspended, no I/O will actually go
// to the zero device, and we are guaranteed the old fd saw all I/O
// from the upper layer before it closed.  Then on a resume request,
// the lower layer does another suspend and resume into the correct
// device, all before the upper layer finally resumes.

// Create the wrappers in the filesystem so that the device can be opened;
// however, I/O to the device is not possible until Resume() is used.
func Create(ctx context.Context, name string, sizeBytes int64) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	// Use of --notable instead of zeroTable() is not portable to older
	// versions of device-mapper.
	_, err := commands.DmsetupCreateIdempotent("--uuid", lowerName(name), "--table", zeroTable(sizeBytes), "--addnodeoncreate", lowerName(name))
	if err != nil {
		log.Error(err, "dm lower create failed")
		return err
	}

	// At least with device-mapper 1.02.199, we have to avoid udev to
	// not hang, at which point not even --addnodeoncreate will create
	// a /dev/mapper entry without a separate mknodes call.  Thankfully,
	// our use of the device does not depend on udev.
	_, err = commands.DmsetupCreateIdempotent("--uuid", upperName(name), "--table", upperTable(sizeBytes, name), "--noudevsync", upperName(name))
	if err != nil {
		log.Error(err, "dm upper create failed")
		_, _ = commands.DmsetupRemoveIdempotent(lowerName(name))
		return err
	}

	_, err = commands.Dmsetup("mknodes", upperName(name))
	if err != nil {
		log.Error(err, "dm upper mknodes failed")
		_ = Remove(ctx, name)
		return err
	}

	// Sanity check that the filesystem devices exist in /dev/mapper now,
	// useful since dmsetup does not accept / in its device names.
	exists, err := commands.PathExistsOnHost(GetDevicePath(name))
	if err == nil && !exists {
		err = errors.NewBadRequest("missing expected dm mapper path")
	}
	if err != nil {
		log.Error(err, "dm mknode failed")
		_ = Remove(ctx, name)
		return err
	}

	return nil
}

// Suspend the device by queuing I/O until the next Resume.  Do not try
// to sync any filesystem if the device is block storage.
func Suspend(ctx context.Context, name string, skipSync bool) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	if exists, err := commands.PathExistsOnHost(GetDevicePath(name)); err == nil && exists {
		// The upper device is not hot-swapping, so no need to
		// force it to fully flush.
		_, err := commands.DmsetupSuspendIdempotent(upperName(name), "--noflush", "--nolockfs")
		if err != nil {
			log.Error(err, "dm upper suspend failed")
			return err
		}

		// The lower layer absolutely must flush (all I/O from
		// before the upper layer suspended must reach the
		// LV).  However, dmsetup should not try to do
		// filesystem magic if this is a block volume.
		args := []string{lowerName(name)}
		if skipSync {
			args = append(args, "--nolockfs")
		}
		_, err = commands.DmsetupSuspendIdempotent(args...)
		if err != nil {
			log.Error(err, "dm lower suspend failed")
			return err
		}

		// Resume the lower layer on a new table, so that the
		// old device is released.  The smaller size doesn't
		// matter, since there will be no I/O anyways.
		_, err = commands.Dmsetup("load", lowerName(name), "--table", zeroTable(512))
		if err != nil {
			log.Error(err, "dm lower load failed")
			return err
		}

		// We already flushed, so another flush is pointless.
		_, err = commands.Dmsetup("resume", "--noflush", "--nolockfs", lowerName(name))
		if err != nil {
			log.Error(err, "dm lower resume failed")
			return err
		}
	}

	return nil
}

// Resume I/O on the volume, as routed through devPath.
func Resume(ctx context.Context, name string, sizeBytes int64, devPath string) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	// No need to flush - the old table should have had no I/O
	// because the upper layer is still suspended.
	_, err := commands.Dmsetup("load", lowerName(name), "--table", lowerTable(sizeBytes, devPath))
	if err != nil {
		log.Error(err, "dm lower load failed")
		return err
	}

	_, err = commands.Dmsetup("resume", "--noflush", "--nolockfs", lowerName(name))
	if err != nil {
		log.Error(err, "dm lower resume failed")
		return err
	}

	// No need to flush - the upper layer is not getting a new table.
	_, err = commands.Dmsetup("resume", "--noflush", "--nolockfs", upperName(name))
	if err != nil {
		log.Error(err, "dm upper resume failed")
		return err
	}

	return nil
}

// Tear down the wrappers.  Should only be called when the device is not in use.
func Remove(ctx context.Context, name string) error {
	log := log.FromContext(ctx).WithValues("nodeName", config.LocalNodeName)

	// --force is necessary to make udev see EIO instead of hanging
	_, err := commands.DmsetupRemoveIdempotent("--force", upperName(name))
	if err != nil {
		log.Error(err, "dm upper remove failed")
		return err
	}

	_, err = commands.DmsetupRemoveIdempotent(lowerName(name))
	if err != nil {
		log.Error(err, "dm lower remove failed")
		return err
	}

	return nil
}

func GetDevicePath(name string) string {
	return "/dev/mapper/" + upperName(name)
}

func lowerName(name string) string {
	return "kubesan-lower-" + name
}

func upperName(name string) string {
	return "kubesan-upper-" + name
}

func zeroTable(sizeBytes int64) string {
	return fmt.Sprintf("0 %d zero", sizeBytes/512)
}

func lowerTable(sizeBytes int64, device string) string {
	return fmt.Sprintf("0 %d linear %s 0", sizeBytes/512, device)
}

func upperTable(sizeBytes int64, name string) string {
	return fmt.Sprintf("0 %d linear /dev/mapper/%s 0", sizeBytes/512, lowerName(name))
}
