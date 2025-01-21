// SPDX-License-Identifier: Apache-2.0

package util

// An error indicating that a Watch is waiting and progress cannot be made
// until Reconcile() is called again.
//
// BlobManager and NBD Server methods do not block when waiting for Kubernetes
// resources. Instead they return WatchPending errors so the caller can handle
// other work in the meantime. When this error is returned, controllers should
// return success from Reconcile() since the runtime will invoke Reconcile()
// again when the Watch triggers.
type WatchPending struct {
	Why string
}

func (w *WatchPending) Error() string {
	if w.Why != "" {
		return "watch pending: " + w.Why
	}
	return "watch pending"
}

// Document why the code should wait for the next Reconcile.
func NewWatchPending(why string) *WatchPending {
	return &WatchPending{Why: why}
}
