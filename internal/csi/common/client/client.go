// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

type CsiK8sClient struct {
	client.Client
	cancel context.CancelFunc
}

func NewCsiK8sClient(cluster bool) (*CsiK8sClient, <-chan struct{}, error) {
	log := log.FromContext(context.Background())
	ctrlOpts := ctrl.Options{
		Scheme: config.Scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				config.Namespace: {},
			},
		},
	}
	log.Info("creating manager")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrlOpts)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "manager failed")
		}
		close(done)
	}()

	clnt := &CsiK8sClient{
		Client: client.NewNamespacedClient(mgr.GetClient(), config.Namespace),
		cancel: cancel,
	}

	// Prime the cache to start watching Volumes and Snapshots by requesting an object that we know won't be found.
	key := client.ObjectKey{Name: "nosuch", Namespace: config.Namespace}
	volume := &v1alpha1.Volume{}
	_ = clnt.Get(ctx, key, volume)
	if cluster {
		snapshot := &v1alpha1.Snapshot{}
		_ = clnt.Get(ctx, key, snapshot)
	}

	return clnt, done, nil
}

// Trigger graceful shutdown of the client.
func (c *CsiK8sClient) Cancel() {
	c.cancel()
}

// Updates `volume` with its last seen state in the cluster. Tries condition once before starting to watch.
func (c *CsiK8sClient) WatchVolumeUntil(ctx context.Context, volume *v1alpha1.Volume, condition func() bool) error {
	return c.watchObjectUntil(ctx, volume, func(err error) (bool, error) {
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return condition(), nil
	})
}

// Updates `snapshot` with its last seen state in the cluster. Tries condition once before starting to watch.
func (c *CsiK8sClient) WatchSnapshotUntil(ctx context.Context, snapshot *v1alpha1.Snapshot, condition func() bool) error {
	return c.watchObjectUntil(ctx, snapshot, func(err error) (bool, error) {
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return condition(), nil
	})
}

// Delete an object, and wait until the deletion is successful.
func (c *CsiK8sClient) DeleteAndConfirm(ctx context.Context, object client.Object) error {
	propagation := client.PropagationPolicy(metav1.DeletePropagationForeground)

	if err := c.Delete(ctx, object, propagation); err != nil {
		return client.IgnoreNotFound(err)
	}

	return c.watchObjectUntil(ctx, object, func(err error) (bool, error) {
		if err == nil {
			return false, nil
		} else if errors.IsNotFound(err) {
			return true, nil
		} else {
			return false, err
		}
	})
}

// Perform an exponential backoff wait on the given object until a condition is met.
func (c *CsiK8sClient) watchObjectUntil(ctx context.Context, object client.Object, condition func(error) (bool, error)) error {
	if done, err := condition(nil); err != nil {
		return err
	} else if done {
		return nil
	}

	log := log.FromContext(ctx).WithValues("object", object.GetName())
	return wait.Backoff{
		Duration: 250 * time.Millisecond,
		Factor:   1.4, // exponential backoff
		Jitter:   0.1,
		Steps:    math.MaxInt,
		Cap:      3 * time.Second,
	}.DelayFunc().Until(ctx, true, false, func(ctx context.Context) (bool, error) {
		log.Info("testing condition")
		return condition(c.Get(ctx, client.ObjectKeyFromObject(object), object))
	})
}
