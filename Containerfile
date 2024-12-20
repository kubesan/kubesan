# SPDX-License-Identifier: Apache-2.0
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

# This file is used to generate the container image to be loaded into
# a KubeSAN deployment.  It assumes that the caller has already prepared
# all necessary generated files; for linting, use tests/Containerfile.
FROM quay.io/projectquay/golang:1.22 AS builder

WORKDIR /kubesan

# Cache the vendoring up front, as that seldom changes.
COPY go.mod go.sum Makefile ./
COPY vendor vendor/
RUN go mod verify

# Copy only source files, not generated binaries, since binaries built
# in the host environment may not work in the container environment.
COPY api/ api/
COPY cmd/ cmd/
COPY deploy/ deploy/
COPY internal/ internal/

# We set GOOS and GOARCH so go cross-compiles to the correct os and arch
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make bin/kubesan

# CentOS Stream 9 doesn't provide package nbd
# FROM quay.io/centos/centos:stream9
# We use --platform=$TARGETPLATFORM to pull the correct arch for
# the base image. This is needed for multi-arch builds
FROM --platform=$TARGETPLATFORM quay.io/fedora/fedora:40

# util-linux-core, e2fsprogs, and xfsprogs are for Filesystem volume support where
# blkid(8) and mkfs are required by k8s.io/mount-utils.
RUN dnf update -y && dnf install --nodocs --noplugins -qy nbd qemu-img util-linux-core e2fsprogs xfsprogs && dnf clean all

WORKDIR /kubesan

COPY --from=builder /kubesan/bin/kubesan bin/

# TODO Should we use a nonroot user?
# USER 65532:65532

ENTRYPOINT [ "/kubesan/bin/kubesan" ]
