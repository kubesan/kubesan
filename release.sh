#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

set -e

GREEN='\033[0;32m'
RESET='\033[0m'
VERSION=$1
PREV_VERSION=$2

if test $# != 2; then
    echo "Usage: $0 VERS PREV_VERS"
    exit 1
fi

# Update files with version number
printf "${GREEN}Updating version number in files${RESET}\n"
sed -i "/Version *= \"${PREV_VERSION}\"/ s/${PREV_VERSION}/${VERSION}/" internal/common/config/config.go
sed -i "/version: / s/latest/${VERSION}/" deploy/kubernetes/kustomization.yaml
sed -i "/newTag: / s/latest/${VERSION}/" deploy/kubernetes/kustomization.yaml
sed -i "s|kubesan/kubesan/deploy/openshift?ref=${PREV_VERSION}|kubesan/kubesan/deploy/openshift?ref=${VERSION}|g" docs/1-getting-started.md
sed -i "s|kubesan/kubesan/deploy/kubernetes?ref=${PREV_VERSION}|kubesan/kubesan/deploy/kubernetes?ref=${VERSION}|g" docs/1-getting-started.md

git add internal/common/config/config.go deploy/kubernetes/kustomization.yaml docs/1-getting-started.md
git commit -s -m "Release ${VERSION}" -e

# Run tests
printf "${GREEN}Running tests${RESET}\n"
tests/run.sh create-cache
tests/run.sh --use-cache all

# Publish container image
printf "${GREEN}Publish container image${RESET}\n"
make container-multiarch CONTAINER_TOOL=podman IMG=quay.io/kubesan/kubesan:${VERSION}
podman manifest push quay.io/kubesan/kubesan:${VERSION}

# Publish git tag
printf "${GREEN}Publishing git tag${RESET}\n"
git tag -s ${VERSION} -m "Release ${VERSION}" && git push origin ${VERSION}

# Revert tag back to 'latest'
printf "${GREEN}Reverting kustomization.yaml tag to 'latest'${RESET}\n"
sed -i "/version: ${VERSION}/ s/version: ${VERSION}/version: latest/" deploy/kubernetes/kustomization.yaml
sed -i "/newTag: ${VERSION}/ s/newTag: ${VERSION}/newTag: latest/" deploy/kubernetes/kustomization.yaml

git add deploy/kubernetes/kustomization.yaml
git commit -s -m "Reopen development after ${VERSION}"

# Push commits
printf "${GREEN}Pushing commits to main${RESET}\n"
git push origin HEAD
