#!/bin/sh
# SPDX-License-Identifier: Apache-2.0

# This file is run by CI

set -e -x

# Build the linting image
podman build -f tests/Containerfile .

# Build and tag the kubesan image
podman build -t kubesan:latest .
