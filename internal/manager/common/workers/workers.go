// SPDX-License-Identifier: Apache-2.0

package workers

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
)

// An item of work that runs in the background and may take a long time to
// complete. Implement this interface to support the Workers.Run() API.
type Work interface {
	// Performs the work, returning nil on success or an error. Invoked
	// from a goroutine. Can time out or be canceled via ctx.
	Run(ctx context.Context) error
}

// Internal state associated with a work invocation
type worker struct {
	// Send nil on success or error on failure
	done chan error

	cancel context.CancelFunc
}

func newWorker() *worker {
	worker := &worker{
		done:   make(chan error, 1),
		cancel: func() {}, // replaced in start()
	}
	return worker
}

// Invoke the work function concurrently
func (w *worker) start(work Work, triggerReconcile func()) {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	go func() {
		w.done <- work.Run(ctx)
		triggerReconcile()
	}()
}

func (w *worker) getResult() (result error, done bool) {
	select {
	case err := <-w.done:
		return err, true
	default:
		return nil, false
	}
}

// An API for background work that triggers the reconcile control loop upon
// completion. Not thread-safe, only use from one reconciler.
type Workers struct {
	Workers map[string]*worker

	// The reconciler is triggered by sending to this channel
	events chan event.GenericEvent
}

func NewWorkers() *Workers {
	return &Workers{
		Workers: make(map[string]*worker),
		events:  make(chan event.GenericEvent),
	}
}

// Runs work in the background. Returns nil if the work completed successfully,
// non-nil if the work resulted in an error, or WatchPending while still in
// progress.
//
// Call this from a reconcile control loop. Most likely it will return
// WatchPending the first time and eventually it will return success or an
// error. Remember to use Cancel() to clean up work that is no longer needed,
// like when a CRD is being deleted.
//
// The name uniquely identifies a work invocation. If no work with the name
// exists then a new one will be started. If work with the name exists and is
// done, then it is removed so that a future call will invoke the worker again.
func (w *Workers) Run(name string, object client.Object, work Work) error {
	worker, ok := w.Workers[name]
	if !ok {
		worker = newWorker()
		w.Workers[name] = worker
		worker.start(work, func() {
			w.events <- event.GenericEvent{Object: object}
		})
	}
	result, done := worker.getResult()
	if !done {
		return util.NewWatchPending("waiting for worker to complete")
	}
	delete(w.Workers, name)
	return result
}

// Returns nil if the worker has completed or WatchPending if still in
// progress. Any error returned by the worker is ignored.
func (w *Workers) Cancel(name string) error {
	worker, ok := w.Workers[name]
	if !ok {
		return nil
	}
	worker.cancel()
	_, done := worker.getResult()
	if !done {
		return util.NewWatchPending("waiting for worker cancellation")
	}
	delete(w.Workers, name)
	return nil
}

func (w *Workers) SetUpReconciler(builder *builder.Builder) {
	builder.WatchesRawSource(
		source.Channel(
			w.events,
			&handler.EnqueueRequestForObject{},
		),
	)
}
