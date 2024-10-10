// SPDX-License-Identifier: Apache-2.0

package cluster

// BlobManager abstracts operations that depend on the volume mode (linear or
// thin).
type BlobManager interface {
	// CreateBlob creates an empty blob of the given size if it does not
	// exist yet.
	//
	// If the blob already exists but the size does not match then it will
	// be recreated with the desired size.
	CreateBlob(name string, sizeBytes int64) error

	// RemoveBlob removes a blob if it exists. No error is returned if the
	// blob does not exist.
	RemoveBlob(name string) error
}
