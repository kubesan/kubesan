// SPDX-License-Identifier: Apache-2.0

package nbd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/digitalocean/go-qemu/qmp"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

const (
	QmpSockPath = "/run/qsd/qmp.sock"
)

// A QEMU Monitor Protocol (QMP) connection to a qemu-storage-daemon instance
// that is running an NBD server.
type qemuStorageDaemonMonitor struct {
	monitor qmp.Monitor
}

// Connect to the monitor of the companion q-s-d container.
func newQemuStorageDaemonMonitor() (*qemuStorageDaemonMonitor, error) {
	monitor, err := qmp.NewSocketMonitor("unix", QmpSockPath, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}

	err = monitor.Connect()
	if err != nil {
		return nil, err
	}

	return &qemuStorageDaemonMonitor{monitor: monitor}, nil
}

// Closes the monitor connection and frees resources.
func (q *qemuStorageDaemonMonitor) Close() {
	_ = q.monitor.Disconnect()
}

// JSON encodes a value. Useful for avoiding escaping issues when expanding
// values into JSON snippets.
func jsonify(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Run a QMP command ignoring error strings containing idempotencyGuard.
func (q *qemuStorageDaemonMonitor) run(ctx context.Context, cmd string, idempotencyGuard string) error {
	log := log.FromContext(ctx)

	log.Info("sending QMP to q-s-d", "command", cmd)
	_, err := q.monitor.Run([]byte(cmd))
	if err != nil {
		if !strings.Contains(err.Error(), idempotencyGuard) {
			log.Info("q-s-d returned failure", "error", err.Error())
			return err
		}
	}
	return nil
}

func (q *qemuStorageDaemonMonitor) BlockdevAdd(ctx context.Context, nodeName string, devicePathOnHost string) error {
	cmd := fmt.Sprintf(`
{
    "execute": "blockdev-add",
    "arguments": {
        "driver": "host_device",
        "node-name": %s,
        "cache": {
            "direct": true
        },
        "filename": %s,
        "aio": "native"
    }
}
`, jsonify(nodeName), jsonify(devicePathOnHost))

	return q.run(ctx, cmd, "Duplicate nodes with node-name")
}

func (q *qemuStorageDaemonMonitor) BlockdevDel(ctx context.Context, nodeName string) error {
	cmd := fmt.Sprintf(`
{
    "execute": "blockdev-del",
    "arguments": { "node-name": %s }
}`, jsonify(nodeName))

	return q.run(ctx, cmd, "Failed to find node with node-name")
}

func (q *qemuStorageDaemonMonitor) BlockExportAdd(ctx context.Context, id string, nodeName string, export string) error {
	cmd := fmt.Sprintf(`
{
    "execute": "block-export-add",
    "arguments": {
        "type": "nbd",
        "id": %s,
        "node-name": %s,
        "writable": true,
        "name": %s
    }
}
`, jsonify(id), jsonify(nodeName), jsonify(export))

	return q.run(ctx, cmd, " is already in use")
}

func (q *qemuStorageDaemonMonitor) BlockExportDel(ctx context.Context, id string) error {
	cmd := fmt.Sprintf(`
{
    "execute": "block-export-del",
    "arguments": {
        "id": %s,
        "mode": "hard"
    }
}
`, jsonify(id))

	return q.run(ctx, cmd, " is not found")
}

// The response to the query-block-exports QMP command
type blockExportInfo struct {
	Id           string `json:"id"`
	Type         string `json:"type"`
	NodeName     string `json:"node-name"`
	ShuttingDown bool   `json:"shutting-down"`
}

func (q *qemuStorageDaemonMonitor) QueryBlockExports(ctx context.Context) ([]blockExportInfo, error) {
	log := log.FromContext(ctx)
	cmd := `{"execute": "query-block-exports"}`
	log.Info("sending QMP to q-s-d", "command", cmd)
	raw, err := q.monitor.Run([]byte(cmd))
	if err != nil {
		return nil, err
	}

	response := struct {
		Return []blockExportInfo `json:"return"`
	}{}
	err = json.Unmarshal(raw, &response)
	if err != nil {
		return nil, err
	}

	return response.Return, nil
}

// Qemu has a tiny 32-byte limit on node names, even though it is okay
// with longer NBD export names.  We need to map incoming export names
// (k8s likes 40-byte pvc-UUID naming) to a shorter string; and our
// mapping can live in local memory.  That is because q-s-d is in the
// same pod as our reconciler code; there is no safe way for a
// DaemonSet to restart our pod while the NBD server is active, so as
// long as we block upgrades until the node is drained of PVC clients,
// a replacement process is always okay to start with fresh numbers
// and a fresh q-s-d instance.  However, we do need to worry about
// concurrent gothreads access.
var blockdevs = struct {
	sync.Mutex
	m     map[string]string
	count uint64
}{m: make(map[string]string)}

// Returns the QMP node name given an NBD export name.
func nodeName(export string) string {
	blockdevs.Lock()
	defer blockdevs.Unlock()

	blockdev, ok := blockdevs.m[export]
	if !ok {
		blockdev = fmt.Sprintf("blockdev-%d", blockdevs.count)
		blockdevs.count++
		blockdevs.m[export] = blockdev
	}
	return blockdev
}

// Returns the QMP block export id given an NBD export name.
func blockExportId(export string) string {
	return "export-" + export
}

// Returns success only once the server is serving the export.
func StartExport(ctx context.Context, export string, devicePathOnHost string) (string, error) {
	qsd, err := newQemuStorageDaemonMonitor()
	if err != nil {
		return "", err
	}
	defer qsd.Close()

	nodeName := nodeName(export)
	err = qsd.BlockdevAdd(ctx, nodeName, devicePathOnHost)
	if err != nil {
		return "", err
	}

	blockExportId := blockExportId(export)
	err = qsd.BlockExportAdd(ctx, blockExportId, nodeName, export)
	if err != nil {
		return "", err
	}

	// Build NBD URI
	url := url.URL{
		Scheme: "nbd",
		Host:   config.PodIP,
		Path:   export,
	}
	return url.String(), nil
}

// Check that export is still being served.
func CheckExportHealth(ctx context.Context, export string) error {
	qsd, err := newQemuStorageDaemonMonitor()
	if err != nil {
		return err
	}
	defer qsd.Close()

	exports, err := qsd.QueryBlockExports(ctx)
	if err != nil {
		return err
	}

	blockExportId := blockExportId(export)
	for i := range exports {
		if exports[i].Id == blockExportId {
			return nil // success
		}
	}

	return k8serrors.NewServiceUnavailable("NBD server unexpectedly gone")
}

// Stop serving the given export.
func StopExport(ctx context.Context, export string) error {
	qsd, err := newQemuStorageDaemonMonitor()
	if err != nil {
		return err
	}
	defer qsd.Close()

	err = qsd.BlockExportDel(ctx, blockExportId(export))
	if err != nil {
		return err
	}

	err = qsd.BlockdevDel(ctx, nodeName(export))
	if err != nil {
		return err
	}

	blockdevs.Lock()
	delete(blockdevs.m, export)
	if Shutdown && len(blockdevs.m) == 0 {
		log := log.FromContext(ctx)
		log.Info("last client closed")
		close(shutdownChannel)
	}
	blockdevs.Unlock()

	return nil
}

// Return true if no new clients should connect to this export
func ExportDegraded(export *v1alpha1.NBDExport) bool {
	return export.Status.URI != "" && !meta.IsStatusConditionTrue(export.Status.Conditions, v1alpha1.NBDExportConditionAvailable)
}

// Return true if this node should stop serving the given export.
func ShouldStopExport(export *v1alpha1.NBDExport, nodes []string, sizeBytes int64) bool {
	if export == nil || export.Spec.Host != config.LocalNodeName {
		return false
	}
	if ExportDegraded(export) {
		return true
	}
	if sizeBytes > export.Spec.SizeBytes {
		return true
	}
	return !slices.Contains(nodes, config.LocalNodeName) || len(nodes) == 1
}

var (
	// Set to true when the NBD server is no longer accepting new clients.
	Shutdown bool

	// Close this channel to indicate when StopServer reacted.
	shutdownChannel = make(chan struct{})
)

// End the server as part of the pod shutdown sequence, via best effort.
// Return a channel that will unblock when NBD has no clients left.
func StopServer(ctx context.Context) <-chan struct{} {
	log := log.FromContext(ctx)
	log.Info("preparing to shut down q-s-d")

	// If there are no clients, we close the channel now; otherwise,
	// it will be closed when the last client stops.
	blockdevs.Lock()
	Shutdown = true
	if len(blockdevs.m) == 0 {
		log.Info("no clients")
		close(shutdownChannel)
	}
	blockdevs.Unlock()

	go func() {
		<-shutdownChannel
		qsd, err := newQemuStorageDaemonMonitor()
		if err != nil {
			log.Error(err, "qemu-storage-daemon shutdown connection failed")
			return
		}
		defer qsd.Close()

		cmd := `{"execute": "quit"}`
		log.Info("sending QMP to q-s-d", "command", cmd)
		if _, err = qsd.monitor.Run([]byte(cmd)); err != nil {
			// This error is likely but harmless, since the monitor
			// may have closed before it got a chance to respond.
			log.Info("ignoring QMP error", "error", err)
		}
	}()

	return shutdownChannel
}
