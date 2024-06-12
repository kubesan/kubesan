// SPDX-License-Identifier: Apache-2.0

package config

import "os"

const (
	Domain  = "subprovisioner.gitlab.io"
	Version = "v0.2.1"

	K8sNamespace = "subprovisioner"
)

var (
	Image         = os.Getenv("SUBPROVISIONER_IMAGE")
	LocalNodeName = os.Getenv("NODE_NAME")
)
