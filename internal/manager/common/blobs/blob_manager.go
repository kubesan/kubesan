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
	// An owner reference may be added from the given owner to a dependent
	// resource associated with the new blob.
	CreateBlob(ctx context.Context, name string, sizeBytes int64, owner client.Object) error

	// RemoveBlob removes a blob if it exists. No error is returned if the
	// blob does not exist.
	RemoveBlob(ctx context.Context, name string) error

	// SnapshotBlob creates a snapshot with a given name from an existing
	// source blob.  If snapshots are not supported, this will fail.
	//
	// An owner reference may be added from the given owner to a dependent
	// resource associated with the snapshot.
	SnapshotBlob(ctx context.Context, name string, sourceName string, owner client.Object) error

	// Returns the size in bytes of the snapshot with the given name and
	// name of the source blob which was passed to SnapshotBlob.
	GetSnapshotSize(ctx context.Context, name string, sourceName string) (int64, error)

	// GetPath returns the matching device name that should exist on
	// any node where the blob is staged.
	GetPath(name string) string
}
