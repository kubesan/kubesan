// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"os"

	"gitlab.com/kubesan/kubesan/internal/csi"
	"gitlab.com/kubesan/kubesan/internal/manager"
)

func badUsage() {
	fmt.Fprintf(os.Stderr, "usage: kubesan csi-controller-plugin [options...]\n")
	fmt.Fprintf(os.Stderr, "       kubesan csi-node-plugin [options...]\n")
	fmt.Fprintf(os.Stderr, "       kubesan cluster-controller-manager [options...]\n")
	fmt.Fprintf(os.Stderr, "       kubesan node-controller-manager [options...]\n")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		badUsage()
	}

	var err error

	// Shuffle out our non-option, so that flag.Parse works on rest of command line
	flavor := os.Args[1]
	os.Args = os.Args[1:]

	switch flavor {
	case "csi-controller-plugin":
		err = csi.RunControllerPlugin()

	case "csi-node-plugin":
		err = csi.RunNodePlugin()

	case "cluster-controller-manager":
		err = manager.RunClusterControllers()

	case "node-controller-manager":
		err = manager.RunNodeControllers()

	default:
		badUsage()
	}

	if err != nil {
		log.Fatalln(err)
	}
}
