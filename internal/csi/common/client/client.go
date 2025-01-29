// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	apimachinerywatch "k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

type CsiK8sClient struct {
	client.Client

	volumeRestClient   rest.Interface
	snapshotRestClient rest.Interface
}

func NewCsiK8sClient() (*CsiK8sClient, error) {
	cfg := ctrlconfig.GetConfigOrDie()

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}

	k8sClient, err := client.New(cfg, client.Options{HTTPClient: httpClient, Scheme: config.Scheme})
	if err != nil {
		return nil, err
	}
	k8sClient = client.NewNamespacedClient(k8sClient, config.Namespace)

	volumeRestClient, err := createRestClient("Volume", cfg, httpClient)
	if err != nil {
		return nil, err
	}

	snapshotRestClient, err := createRestClient("Snapshot", cfg, httpClient)
	if err != nil {
		return nil, err
	}

	client := &CsiK8sClient{
		Client:             k8sClient,
		volumeRestClient:   volumeRestClient,
		snapshotRestClient: snapshotRestClient,
	}

	return client, nil
}

func createRestClient(kind string, cfg *rest.Config, httpClient *http.Client) (rest.Interface, error) {
	return apiutil.RESTClientForGVK(
		v1alpha1.GroupVersion.WithKind(kind),
		false,
		cfg,
		serializer.NewCodecFactory(config.Scheme),
		httpClient,
	)
}

// Updates `volume` with its last seen state in the cluster. Tries condition once before starting to watch.
func (c *CsiK8sClient) WatchVolumeUntil(ctx context.Context, volume *v1alpha1.Volume, condition func() bool) error {
	return c.TryWatchVolumeUntil(ctx, volume, func() (bool, error) { return condition(), nil })
}

// Updates `volume` with its last seen state in the cluster.
func (c *CsiK8sClient) TryWatchVolumeUntil(ctx context.Context, volume *v1alpha1.Volume, condition func() (bool, error)) error {
	if done, err := condition(); err != nil {
		return err
	} else if done {
		return nil
	}

	lw := cache.NewListWatchFromClient(
		c.volumeRestClient,
		"volumes",
		config.Namespace,
		fields.OneTermEqualSelector("metadata.name", volume.Name),
	)

	cond := func(event apimachinerywatch.Event) (bool, error) {
		event.Object.(*v1alpha1.Volume).DeepCopyInto(volume)
		return condition()
	}

	_, err := watch.UntilWithSync(ctx, lw, &v1alpha1.Volume{}, nil, cond)
	return err
}

// Updates `snapshot` with its last seen state in the cluster. Tries condition once before starting to watch.
func (c *CsiK8sClient) WatchSnapshotUntil(ctx context.Context, snapshot *v1alpha1.Snapshot, condition func() bool) error {
	return c.TryWatchSnapshotUntil(ctx, snapshot, func() (bool, error) { return condition(), nil })
}

// Updates `snapshot` with its last seen state in the cluster.
func (c *CsiK8sClient) TryWatchSnapshotUntil(ctx context.Context, snapshot *v1alpha1.Snapshot, condition func() (bool, error)) error {
	if done, err := condition(); err != nil {
		return err
	} else if done {
		return nil
	}

	lw := cache.NewListWatchFromClient(
		c.snapshotRestClient,
		"snapshots",
		config.Namespace,
		fields.OneTermEqualSelector("metadata.name", snapshot.Name),
	)

	cond := func(event apimachinerywatch.Event) (bool, error) {
		event.Object.(*v1alpha1.Snapshot).DeepCopyInto(snapshot)
		return condition()
	}

	_, err := watch.UntilWithSync(ctx, lw, &v1alpha1.Snapshot{}, nil, cond)
	return err
}
