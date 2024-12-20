# SPDX-License-Identifier: Apache-2.0

# Image URL to use all building/pushing image targets
IMG ?= kubesan:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.29.0

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.14.0
ENVTEST_VERSION ?= release-0.17
GOLANGCI_LINT_VERSION ?= v1.62.2

## Tool Binaries
KUBECTL ?= kubectl
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Set CONTAINER_PLATFORMS=--all-platforms to see if this static list needs to grow
CONTAINER_TOOL ?= podman
CONTAINER_PLATFORMS ?= --platform=linux/amd64,linux/ppc64le,linux/s390x,linux/arm64

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: .generate.timestamp vet lint bin/kubesan ## Generate files and build the KubeSAN image locally.

# Even though bin/kubesan is a real file, we don't want to spell out all of
# the .go file dependencies; it's easier to just run go build unconditionally.
.PHONY: bin/kubesan
bin/kubesan: ## Build just the KubeSAN image, locally or in a container.
	go build -mod=vendor --ldflags "-s -w" -a -o bin/kubesan cmd/main.go

.PHONY: container
container: ## Build a single-arch KubeSAN container, for testing.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: container-multiarch
container-multiarch: ## Build a multi-arch KubeSAN container, for release.
	$(CONTAINER_TOOL) manifest create --amend $(IMG)
	$(CONTAINER_TOOL) build $(CONTAINER_PLATFORMS) --manifest $(IMG) .

##@ Development

.PHONY: generate
generate: controller-gen .generate.timestamp

GROUPVERSION_INFO=api/v1alpha1/groupversion_info.go
CRD_TYPES:=$(wildcard api/v1alpha1/*_types.go)

.generate.timestamp: $(CONTROLLER_GEN) $(GROUPVERSION_INFO) $(CRD_TYPES)  ## Generate ClusterRole and CustomResourceDefinition YAML, and corresponding DeepCopy*() methods.
	$(CONTROLLER_GEN) \
		object:headerFile="hack/header.go.txt" \
		crd:headerFile="hack/header.yaml.txt" \
		rbac:roleName=manager-role,headerFile="hack/header.yaml.txt" \
		output:crd:artifacts:config=deploy/kubernetes/crd \
		output:rbac:artifacts:config=deploy/kubernetes/rbac \
		paths="./..."
	touch $@

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test-unit
test-unit: vet lint envtest ## Run unit tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./...

.PHONY: test-e2e
test-e2e: vet lint ## Run end-to-end tests (tests/run.sh all).
	tests/run.sh all

.PHONY: lint
lint: golangci-lint ## Run the golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run the golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: tidy
tidy:  ## Clean up go.mod.
	go mod tidy

.PHONY: vendor
vendor: tidy  ## Update the vendor directory.
	go mod vendor

##@ Dependencies

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,${GOLANGCI_LINT_VERSION})

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install -mod=readonly $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef
