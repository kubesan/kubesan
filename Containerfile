# SPDX-License-Identifier: Apache-2.0

FROM quay.io/projectquay/golang:1.20 AS builder

WORKDIR /subprovisioner

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN go build -o bin/subprovisioner ./cmd/subprovisioner

# CentOS Stream 9 doesn't provide package nbd
# FROM quay.io/centos/centos:stream9
FROM quay.io/fedora/fedora:39

RUN dnf install -qy lvm2-lockd nbd sanlock && dnf clean all

WORKDIR /subprovisioner

# prevent LVM commands from failing due to thinking that lvmlockd isn't running
RUN touch /run/lvmlockd.pid

COPY lvm/lvm.conf /etc/lvm/
COPY lvm/*.sh ./
COPY nbd/*.sh ./

COPY --from=builder /subprovisioner/bin/subprovisioner ./

ENTRYPOINT []
