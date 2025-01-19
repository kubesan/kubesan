// SPDX-License-Identifier: Apache-2.0

package nbd

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	kubesanslices "gitlab.com/kubesan/kubesan/internal/common/slices"
	"gitlab.com/kubesan/kubesan/internal/manager/common/util"
)

// nbd-client is not idempotent (it allocates a new device each time
// it is called with the same export destination), so we have to track
// things to avoid duplication, via a map of export URIs to their
// matching /dev/nbdN devices.  Access may be from parallel
// goroutines, hence we also need to use a mutex.
var clients = struct {
	sync.Mutex
	m map[string]string
}{m: make(map[string]string)}

// Populate the map with all NBD devices currently connected to an export,
// useful for rebuilding state when restarting the manager pod.
func Startup() {
	clients.Lock()
	defer clients.Unlock()

	log := log.FromContext(context.Background())

	// Ignore NBD devices that have a backend we don't recognize,
	// as they were probably not created by KubeSAN.
	devices, _ := filepath.Glob("/sys/block/nbd*/backend")

	for _, path := range devices {
		dev, _ := strings.CutPrefix(path, "/sys/block/")
		dev, _ = strings.CutSuffix(dev, "/backend")
		uri, err := commands.NBDClientBackend(dev)
		if err == nil && strings.HasPrefix(uri, "nbd://") {
			log.Info("Inheriting device", "device", "/dev/"+dev, "uri", uri)
			clients.m[uri] = dev
		}
	}
	log.Info("Found pre-existing NBD clients", "count", len(clients.m))
}

// Connect to the given NBDExport and return the device path.
// Fail with WatchPending if the export is not serving.
func ConnectClient(ctx context.Context, c client.Client, export *v1alpha1.NBDExport) (string, error) {
	if export == nil || export.Status.URI == "" || ExportDegraded(export) {
		return "", util.NewWatchPending("waiting for NBD server to become available")
	}

	clients.Lock()
	defer clients.Unlock()

	if !slices.Contains(export.Spec.Clients, config.LocalNodeName) {
		export.Spec.Clients = append(export.Spec.Clients, config.LocalNodeName)
		if err := c.Update(ctx, export); err != nil {
			return "", err
		}
	}
	dev, ok := clients.m[export.Status.URI]
	if !ok {
		var err error
		dev, err = commands.NBDClientConnect(export.Status.URI)
		if err != nil {
			// If we failed to connect, make a best effort to
			// undo the Spec change; but the design works even
			// if the spec claim lives on.
			export.Spec.Clients = kubesanslices.RemoveAll(export.Spec.Clients, config.LocalNodeName)
			_ = c.Update(ctx, export)
			return "", err
		}
		clients.m[export.Status.URI] = dev
	}
	return "/dev/" + dev, nil
}

// Check if this node is currently connected as a client to the named export.
// Returns two bools: the first says whether there is a connection, the
// second is set if disconnection is required.
func CheckClient(ctx context.Context, c client.Client, export *v1alpha1.NBDExport) (bool, bool) {
	if export == nil || export.Status.URI == "" {
		return false, false
	}

	clients.Lock()
	defer clients.Unlock()

	dev, ok := clients.m[export.Status.URI]
	if !ok {
		return false, slices.Contains(export.Spec.Clients, config.LocalNodeName)
	}
	uri, err := commands.NBDClientBackend(dev)
	return true, err != nil || uri != export.Status.URI || ExportDegraded(export)
}

// Disconnect from the named NBDExport.
func DisconnectClient(ctx context.Context, c client.Client, export *v1alpha1.NBDExport) error {
	if export == nil || !slices.Contains(export.Spec.Clients, config.LocalNodeName) {
		return nil
	}

	clients.Lock()
	defer clients.Unlock()

	dev, ok := clients.m[export.Status.URI]
	if ok {
		if err := commands.NBDClientDisconnect(export.Status.URI, dev); err != nil {
			return err
		}
	}

	delete(clients.m, export.Status.URI)
	export.Spec.Clients = kubesanslices.RemoveAll(export.Spec.Clients, config.LocalNodeName)
	return c.Update(ctx, export)
}
