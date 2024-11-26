// SPDX-License-Identifier: Apache-2.0

package blobs

import "context"

// BlobManager abstracts operations that depend on the volume mode (linear or
// thin).
type BlobManager interface {
	// CreateBlob creates an empty blob of the given size if it does not
	// exist yet.
	//
	// If the blob already exists but the size does not match then it will
	// be recreated with the desired size.
	CreateBlob(ctx context.Context, name string, sizeBytes int64) error

	// RemoveBlob removes a blob if it exists. No error is returned if the
	// blob does not exist.
	RemoveBlob(ctx context.Context, name string) error

	// GetPath returns the matching device name that should exist on
	// any node where the blob is staged.
	GetPath(name string) string
}
