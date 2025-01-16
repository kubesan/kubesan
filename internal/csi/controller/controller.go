// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"

	csiclient "gitlab.com/kubesan/kubesan/internal/csi/common/client"
)

type ControllerServer struct {
	csi.UnimplementedControllerServer

	client *csiclient.CsiK8sClient
}

func NewControllerServer(client *csiclient.CsiK8sClient) *ControllerServer {
	return &ControllerServer{
		client: client,
	}
}

func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		// TODO Enable these once snapshot and clone are implemented
		//		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_VOLUME_CONDITION,
		csi.ControllerServiceCapability_RPC_GET_VOLUME,
	}

	csiCaps := make([]*csi.ControllerServiceCapability, len(caps))
	for i, cap := range caps {
		csiCaps[i] = &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}

	return resp, nil
}
