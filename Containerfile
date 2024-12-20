# SPDX-License-Identifier: Apache-2.0
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

# This file is used to generate the container image to be loaded into
# a KubeSAN deployment.  It assumes that the caller has already prepared
# all necessary generated files; for linting, use tests/Containerfile.

## --target target
# This is the default target.  It is pulled first to ensure we have the
# right architectures, while still allowing the builder to run natively.
# CentOS Stream 9 doesn't provide package nbd
# FROM quay.io/centos/centos:stream9
FROM --platform=$TARGETPLATFORM quay.io/fedora/fedora:40 as target

## --target builder
# This target builds the kubesan binary, possibly with cross-compilation.
FROM --platform=$BUILDPLATFORM quay.io/projectquay/golang:1.22 AS builder

WORKDIR /kubesan

# Cache the vendoring up front, as that seldom changes.
COPY go.mod go.sum Makefile ./
COPY vendor vendor/
RUN go mod verify

# Copy only source files, not generated binaries, since binaries built
# in the host environment may not work in the container environment.
# Sort directories with the least-frequently updated ones first.
COPY api/ api/
COPY deploy/ deploy/
COPY cmd/ cmd/
COPY internal/ internal/

# We set GOOS and GOARCH so go cross-compiles to the correct os and arch
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make bin/kubesan

## Back to the default target
FROM target

# util-linux-core, e2fsprogs, and xfsprogs are for Filesystem volume support where
# blkid(8) and mkfs are required by k8s.io/mount-utils.
RUN dnf update -y && dnf install --nodocs --noplugins -qy nbd qemu-img util-linux-core e2fsprogs xfsprogs && dnf clean all

WORKDIR /kubesan

COPY --from=builder /kubesan/bin/kubesan bin/

# TODO Should we use a nonroot user?
# USER 65532:65532

ENTRYPOINT [ "/kubesan/bin/kubesan" ]
