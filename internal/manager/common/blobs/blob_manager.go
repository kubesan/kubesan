// SPDX-License-Identifier: Apache-2.0

package blobs

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BlobManager abstracts operations that depend on the volume mode (linear or
// thin).
type BlobManager interface {
	// CreateBlob creates an empty blob of the given size if it does not
	// exist yet.
	//
	// If the blob already exists but the size does not match then it will
	// be expanded to the desired size.
	//
	// If skipWipe is true, it is not necessary to wipe the entire blob;
	// however, the first 4Mi must read as zero on creation, to ensure
	// any filesystem can be safely installed.
	//
	// An owner reference may be added from the given owner to a dependent
	// resource associated with the new blob.
	CreateBlob(ctx context.Context, name string, binding string, sizeBytes int64, skipWipe bool, owner client.Object) error

	// RemoveBlob removes a blob if it exists. No error is returned if the
	// blob does not exist. An owner reference may be removed from the
	// given owner to a dependent resource associated with the blob.
	RemoveBlob(ctx context.Context, name string, owner client.Object) error

	// SnapshotBlob creates a snapshot with a given name from an existing
	// source blob.  If snapshots are not supported, this will fail.
	//
	// An owner reference may be added from the given owner to a dependent
	// resource associated with the snapshot.
	SnapshotBlob(ctx context.Context, name string, binding string, sourceName string, owner client.Object) error

	// ActivateBlobForCloneSource activates a data source blob so it can be
	// cloned. The owner may be updated to record that it depends on the
	// data source. Make sure to call DeactivateBlobForCloneSource to clean
	// up later.
	ActivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error

	// ActivateBlobForCloneTarget activates a cloning target blob. The data
	// source blob's BlobManager must be given so the target blob can be
	// activated on the same node as the source blob. Make sure to call
	// DeactivateBlobForCloneTarget to clean up later.  Returns the node
	// name where activation took place.
	ActivateBlobForCloneTarget(ctx context.Context, name string, dataSrcBlobMgr BlobManager) (string, error)

	// DeactivateBlobForCloneSource deactivates a data source blob that was
	// used for cloning. See ActivateBlobForCloneSource.
	DeactivateBlobForCloneSource(ctx context.Context, name string, owner client.Object) error

	// DeactivateBlobForCloneTarget deactivates a cloning target blob. See
	// ActivateBlobForCloneTarget.
	DeactivateBlobForCloneTarget(ctx context.Context, name string) error

	// Returns the (logical) size in bytes of the blob.
	GetSize(ctx context.Context, name string) (int64, error)

	// GetPath returns the matching device name that should exist on
	// any node where the blob is staged.
	GetPath(name string) string

	// Return true if expansion requires the blob to be offline first.
	ExpansionMustBeOffline() bool

	// Return true if the caller should check if Status.SizeBytes
	// needs updating, based on whether the Blob is currently staged.
	SizeNeedsCheck(staged bool) bool
}
