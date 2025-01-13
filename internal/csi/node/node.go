// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"

	"gitlab.com/kubesan/kubesan/internal/common/config"
	csiclient "gitlab.com/kubesan/kubesan/internal/csi/common/client"
)

type NodeServer struct {
	csi.UnimplementedNodeServer

	client *csiclient.CsiK8sClient

	// Keep around instances to avoid the cost of probing in mount.New("")
	// each time an instance is required.
	mounter mount.Interface
	exec    exec.Interface
}

func NewNodeServer(client *csiclient.CsiK8sClient) *NodeServer {
	return &NodeServer{
		client:  client,
		mounter: mount.New(""),
		exec:    exec.New(),
	}
}

func (s *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{
		NodeId: config.LocalNodeName,
	}

	return resp, nil
}

func (s *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
	}

	csiCaps := make([]*csi.NodeServiceCapability, len(caps))
	for i, cap := range caps {
		csiCaps[i] = &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	resp := &csi.NodeGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}

	return resp, nil
}
