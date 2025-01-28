// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"gitlab.com/kubesan/kubesan/api/v1alpha1"
)

const (
	Domain = "kubesan.gitlab.io"

	// + Don't forget to update deploy/kubernetes/kustomization.yaml
	// + when bumping this version string for a release.
	Version = "v0.10.0"

	Finalizer = Domain + "/finalizer"

	CloneSourceLabel = Domain + "/cloneSource"

	CsiSocketPath = "/run/csi/socket"

	LvmProfileName = "kubesan"
	LvmProfile     = "" +
		"# This file is part of the KubeSAN CSI plugin and may be automatically\n" +
		"# updated. Do not edit!\n" +
		"\n" +
		"activation {\n" +
		"        thin_pool_autoextend_threshold=95\n" +
		"        thin_pool_autoextend_percent=20\n" +
		"}\n"
)

var (
	LocalNodeName = os.Getenv("NODE_NAME")
	PodName       = os.Getenv("POD_NAME")
	PodIP         = os.Getenv("POD_IP")

	Namespace string

	Scheme = runtime.NewScheme()

	ErrCannotDetermineNamespace = errors.New("could not determine namespace from service account or WATCH_NAMESPACE env var")
)

func init() {
	Namespace = getNamespace()

	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))

	utilruntime.Must(v1alpha1.AddToScheme(Scheme))
	// +kubebuilder:scaffold:scheme
}

func getNamespace() string {
	namespace, present := os.LookupEnv("WATCH_NAMESPACE")
	if present {
		return namespace
	}

	fi, err := os.Open("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		panic(errors.Join(ErrCannotDetermineNamespace, err))
	}
	defer func(fi *os.File) {
		if err := fi.Close(); err != nil {
			panic(errors.Join(ErrCannotDetermineNamespace, err))
		}
	}(fi)

	data, err := io.ReadAll(fi)
	if err != nil {
		panic(errors.Join(ErrCannotDetermineNamespace, err))
	}

	return string(data)
}
