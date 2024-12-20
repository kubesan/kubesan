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
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/digitalocean/go-qemu/qmp"

	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
	"gitlab.com/kubesan/kubesan/internal/common/config"
)

const (
	QmpSockPath = "/run/qsd/qmp.sock"
)

// The Volume code will name the NBDExport CR Node-Volume-thin (where
// Volume==Export); but it is easier to pass items explicitly than trying
// to parse items out of a CR name.
type ServerId struct {
	// The node to which the server should be scheduled.
	Node string

	// The export name
	Export string
}

// A QEMU Monitor Protocol (QMP) connection to a qemu-storage-daemon instance
// that is running an NBD server.
type qemuStorageDaemonMonitor struct {
	monitor qmp.Monitor
}

func newQemuStorageDaemonMonitor(path string) (*qemuStorageDaemonMonitor, error) {
	monitor, err := qmp.NewSocketMonitor("unix", path, 100*time.Millisecond)
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

// Returns success only once the server is running and has the TCP port open.
func StartServer(ctx context.Context, id *ServerId, devicePathOnHost string) (string, error) {
	qsd, err := newQemuStorageDaemonMonitor(QmpSockPath)
	if err != nil {
		return "", err
	}
	defer qsd.Close()

	nodeName := nodeName(id.Export)
	err = qsd.BlockdevAdd(ctx, nodeName, devicePathOnHost)
	if err != nil {
		return "", err
	}

	blockExportId := blockExportId(id.Export)
	err = qsd.BlockExportAdd(ctx, blockExportId, nodeName, id.Export)
	if err != nil {
		return "", err
	}

	// Build NBD URI
	url := url.URL{
		Scheme: "nbd",
		Host:   config.PodIP,
		Path:   id.Export,
	}
	return url.String(), nil
}

func CheckServerHealth(ctx context.Context, id *ServerId) error {
	qsd, err := newQemuStorageDaemonMonitor(QmpSockPath)
	if err != nil {
		return err
	}
	defer qsd.Close()

	exports, err := qsd.QueryBlockExports(ctx)
	if err != nil {
		return err
	}

	blockExportId := blockExportId(id.Export)
	for i := range exports {
		if exports[i].Id == blockExportId {
			return nil // success
		}
	}

	return k8serrors.NewServiceUnavailable("NBD server unexpectedly gone")
}

func StopServer(ctx context.Context, id *ServerId) error {
	qsd, err := newQemuStorageDaemonMonitor(QmpSockPath)
	if err != nil {
		return err
	}
	defer qsd.Close()

	err = qsd.BlockExportDel(ctx, blockExportId(id.Export))
	if err != nil {
		return err
	}

	err = qsd.BlockdevDel(ctx, nodeName(id.Export))
	if err != nil {
		return err
	}

	blockdevs.Lock()
	delete(blockdevs.m, id.Export)
	blockdevs.Unlock()

	return nil
}

// Return true if no new clients should connect to this export
func ExportDegraded(export *v1alpha1.NBDExport) bool {
	return export.Status.URI != "" && !conditionsv1.IsStatusConditionTrue(export.Status.Conditions, conditionsv1.ConditionAvailable)
}

// Return true if this node should stop serving the given export.
func ShouldStopServer(export *v1alpha1.NBDExport, nodes []string) bool {
	if export == nil || export.Spec.Host != config.LocalNodeName {
		return false
	}
	if ExportDegraded(export) {
		return true
	}
	return !slices.Contains(nodes, config.LocalNodeName) || (len(nodes) == 1 && nodes[0] == config.LocalNodeName)
}
