# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the node-problem-detector image.

.PHONY: all \
        lint vet fmt version test e2e-test \
        build-binaries build-container build-tar build \
        docker-builder build-in-docker \
        push-container push-tar push release clean depup \
        print-tar-sha-md5

all: build

# PLATFORMS is the set of OS_ARCH that NPD can build against.
LINUX_PLATFORMS=linux_amd64 linux_arm64
DOCKER_PLATFORMS=linux/amd64,linux/arm64
PLATFORMS=$(LINUX_PLATFORMS) windows_amd64

# BRANCH is the git branch.
BRANCH=$(shell git symbolic-ref --short HEAD)

# VERSION is the git version of the binary.
VERSION?=$(shell git describe --tags --always --dirty)

# TAG is the tag of the container image, default to binary version.
TAG?=$(VERSION)

# REGISTRY is the container registry to push into.
REGISTRY?=gcr.io/k8s-staging-npd

# UPLOAD_PATH is the cloud storage path to upload release tar.
UPLOAD_PATH?=gs://kubernetes-release
# Trim the trailing '/' in the path
UPLOAD_PATH:=$(shell echo $(UPLOAD_PATH) | sed '$$s/\/*$$//')

# PKG is the package name of node problem detector repo.
PKG:=k8s.io/node-problem-detector

# PKG_SOURCES are all the go source code.
ifeq ($(OS),Windows_NT)
PKG_SOURCES:=
# TODO: File change detection does not work in Windows.
else
PKG_SOURCES:=$(shell find pkg cmd -name '*.go')
endif

# PARALLEL specifies the number of parallel test nodes to run for e2e tests.
PARALLEL?=3

NPD_NAME_VERSION?=node-problem-detector-$(VERSION)
# TARBALL is the name of release tar. Include binary version by default.
TARBALL=$(NPD_NAME_VERSION).tar.gz

# IMAGE_TAGS contains the image tags of the node problem detector container image.
IMAGE_TAGS=--tag $(REGISTRY)/node-problem-detector:$(TAG)
IMAGE_TAGS_WINDOWS=--tag $(REGISTRY)/node-problem-detector-windows:$(TAG)
ifeq ($(REGISTRY), gcr.io/k8s-staging-npd)
ifeq (,$(findstring heads,$(BRANCH)))
  IMAGE_TAGS+= --tag $(REGISTRY)/node-problem-detector:$(BRANCH)
  IMAGE_TAGS_WINDOWS+= --tag $(REGISTRY)/node-problem-detector-windows:$(BRANCH)
endif
endif

# ENABLE_JOURNALD enables build journald support or not. Building journald
# support needs libsystemd-dev or libsystemd-journal-dev.
ENABLE_JOURNALD?=1

ifeq ($(shell go env GOHOSTOS), darwin)
ENABLE_JOURNALD=0
else ifeq ($(shell go env GOHOSTOS), windows)
ENABLE_JOURNALD=0
endif

# Disable cgo by default to make the binary statically linked.
CGO_ENABLED:=0

ifeq ($(GOARCH), arm64)
	CC:=aarch64-linux-gnu-gcc
else
	CC:=x86_64-linux-gnu-gcc
endif

# Set default Go architecture to AMD64.
GOARCH ?= amd64

# Construct the "-tags" parameter used by "go build".
BUILD_TAGS?=

LINUX_BUILD_TAGS = $(BUILD_TAGS)
WINDOWS_BUILD_TAGS = $(BUILD_TAGS)

ifeq ($(OS),Windows_NT)
HOST_PLATFORM_BUILD_TAGS = $(WINDOWS_BUILD_TAGS)
else
HOST_PLATFORM_BUILD_TAGS = $(LINUX_BUILD_TAGS)
endif

ifeq ($(ENABLE_JOURNALD), 1)
	# Enable journald build tag.
	LINUX_BUILD_TAGS := journald $(BUILD_TAGS)
	# Enable cgo because sdjournal needs cgo to compile. The binary will be
	# dynamically linked if CGO_ENABLED is enabled. This is fine because fedora
	# already has necessary dynamic library. We can not use `-extldflags "-static"`
	# here, because go-systemd uses dlopen, and dlopen will not work properly in a
	# statically linked application.
	CGO_ENABLED:=1
	LOGCOUNTER=./bin/log-counter
else
	# Hack: Don't copy over log-counter, use a wildcard path that shouldn't match
	# anything in COPY command.
	LOGCOUNTER=*dont-include-log-counter
endif

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT := ./.bin/golangci-lint

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --config .golangci.yml ./...

$(GOLANGCI_LINT):
	@echo "golangci-lint not found, downloading..."
	@mkdir -p ./.bin
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b ./.bin $(GOLANGCI_LINT_VERSION)


vet:
	go list -tags "$(HOST_PLATFORM_BUILD_TAGS)" ./... | \
		grep -v "./vendor/*" | \
		xargs go vet -tags "$(HOST_PLATFORM_BUILD_TAGS)"

fmt: $(GOLANGCI_LINT)
	find . -type f -name "*.go" | grep -v "./vendor/*" | xargs gofmt -s -w -l
	$(GOLANGCI_LINT) run --config .golangci.yml --fix ./...

version:
	@echo $(VERSION)

BINARIES = bin/node-problem-detector bin/health-checker test/bin/problem-maker
BINARIES_LINUX_ONLY =
ifeq ($(ENABLE_JOURNALD), 1)
	BINARIES_LINUX_ONLY += bin/log-counter
endif

ALL_BINARIES = $(foreach binary, $(BINARIES) $(BINARIES_LINUX_ONLY), ./$(binary)) \
  $(foreach platform, $(LINUX_PLATFORMS), $(foreach binary, $(BINARIES) $(BINARIES_LINUX_ONLY), output/$(platform)/$(binary))) \
  $(foreach binary, $(BINARIES), output/windows_amd64/$(binary).exe)
ALL_TARBALLS = $(foreach platform, $(PLATFORMS), $(NPD_NAME_VERSION)-$(platform).tar.gz)

output/windows_amd64/bin/%.exe: $(PKG_SOURCES)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build \
		-o $@ \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(WINDOWS_BUILD_TAGS)" \
		./cmd/$(subst -,,$*)
	touch $@

output/windows_amd64/test/bin/%.exe: $(PKG_SOURCES)
	cd test && \
	GOOS=windows GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build \
		-o ../$@ \
		-tags "$(WINDOWS_BUILD_TAGS)" \
		./e2e/$(subst -,,$*)

output/linux_amd64/bin/%: $(PKG_SOURCES)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) \
	  CC=x86_64-linux-gnu-gcc go build \
		-o $@ \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(LINUX_BUILD_TAGS)" \
		./cmd/$(subst -,,$*)
	touch $@

output/linux_amd64/test/bin/%: $(PKG_SOURCES)
	cd test && \
	GOOS=linux GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) \
	  CC=x86_64-linux-gnu-gcc go build \
		-o ../$@ \
		-tags "$(LINUX_BUILD_TAGS)" \
		./e2e/$(subst -,,$*)

output/linux_arm64/bin/%: $(PKG_SOURCES)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) \
	  CC=aarch64-linux-gnu-gcc go build \
		-o $@ \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(LINUX_BUILD_TAGS)" \
		./cmd/$(subst -,,$*)
	touch $@

output/linux_arm64/test/bin/%: $(PKG_SOURCES)
	cd test && \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) \
	  CC=aarch64-linux-gnu-gcc go build \
		-o ../$@ \
		-tags "$(LINUX_BUILD_TAGS)" \
		./e2e/$(subst -,,$*)

# In the future these targets should be deprecated.
./bin/log-counter: $(PKG_SOURCES)
ifeq ($(ENABLE_JOURNALD), 1)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=$(GOARCH) CC=$(CC) go build \
		-o bin/log-counter \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(LINUX_BUILD_TAGS)" \
		cmd/logcounter/log_counter.go
else
	echo "Warning: log-counter requires journald, skipping."
endif

./bin/node-problem-detector.exe: $(PKG_SOURCES)
	CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build \
		-o bin/node-problem-detector.exe \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		./cmd/nodeproblemdetector

./bin/node-problem-detector: $(PKG_SOURCES)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=$(GOARCH) CC=$(CC) go build \
		-o bin/node-problem-detector \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(LINUX_BUILD_TAGS)" \
		./cmd/nodeproblemdetector

./test/bin/problem-maker: $(PKG_SOURCES)
	cd test && \
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=$(GOARCH) CC=$(CC) go build \
		-o bin/problem-maker \
		-tags "$(LINUX_BUILD_TAGS)" \
		./e2e/problemmaker/problem_maker.go

./bin/health-checker: $(PKG_SOURCES)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=$(GOARCH) CC=$(CC) go build \
		-o bin/health-checker \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		-tags "$(LINUX_BUILD_TAGS)" \
		cmd/healthchecker/health_checker.go

./bin/health-checker.exe: $(PKG_SOURCES)
	CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build \
		-o bin/health-checker.exe \
		-ldflags '-X $(PKG)/pkg/version.version=$(VERSION)' \
		cmd/healthchecker/health_checker.go

test: vet fmt
	go test -timeout=1m -v -race -short -tags "$(HOST_PLATFORM_BUILD_TAGS)" ./...

e2e-test: vet fmt build-tar
	cd test && \
	go run github.com/onsi/ginkgo/ginkgo -nodes=$(PARALLEL) -timeout=10m -v -tags "$(HOST_PLATFORM_BUILD_TAGS)" -stream \
	./e2e/metriconly/... -- \
	-project=$(PROJECT) -zone=$(ZONE) \
	-image=$(VM_IMAGE) -image-family=$(IMAGE_FAMILY) -image-project=$(IMAGE_PROJECT) \
	-ssh-user=$(SSH_USER) -ssh-key=$(SSH_KEY) \
	-npd-build-tar=`pwd`/../$(TARBALL) \
	-boskos-project-type=$(BOSKOS_PROJECT_TYPE) -job-name=$(JOB_NAME) \
	-artifacts-dir=$(ARTIFACTS)

$(NPD_NAME_VERSION)-%.tar.gz: $(ALL_BINARIES) test/e2e-install.sh
	mkdir -p output/$*/ output/$*/test/
	cp -r config/ output/$*/
	cp test/e2e-install.sh output/$*/test/e2e-install.sh
	(cd output/$*/ && tar -zcvf ../../$@ *)
	sha512sum $@ > $@.sha512

image-$(NPD_NAME_VERSION)-linux_%.tar.gz: output/linux_%/test/bin/problem-maker test/e2e-install.sh
	mkdir -p output/linux_$*/bin output/linux_$*/test
	docker create --name npd-$* --platform linux/$* registry.k8s.io/node-problem-detector/node-problem-detector:$(TAG)
	docker cp npd-$*:/node-problem-detector output/linux_$*/bin/
	docker cp npd-$*:/home/kubernetes/bin/health-checker output/linux_$*/bin/
	docker cp npd-$*:/home/kubernetes/bin/log-counter output/linux_$*/bin/
	docker cp npd-$*:/config output/linux_$*/
	docker rm -v npd-$*
	cp test/e2e-install.sh output/linux_$*/test/e2e-install.sh
	(cd output/linux_$*/ && tar -zcvf ../../$@ *)
	cp $@ $(NPD_NAME_VERSION)-linux_$*.tar.gz
	sha512sum $(NPD_NAME_VERSION)-linux_$*.tar.gz > $(NPD_NAME_VERSION)-linux_$*.tar.gz.sha512

image-$(NPD_NAME_VERSION)-windows_%.tar.gz: output/windows_%/test/bin/problem-maker.exe test/e2e-install.sh
	mkdir -p output/windows_$*/bin output/windows_$*/test/
	docker create --name npd-$* --platform windows/$* registry.k8s.io/node-problem-detector/node-problem-detector-windows:$(TAG)
	docker cp npd-$*:/Files/node-problem-detector.exe output/windows_$*/bin/
	docker cp npd-$*:/Files/etc/kubernetes/node/bin/health-checker.exe output/windows_$*/bin/
	docker cp npd-$*:/Files/config output/windows_$*/
	docker rm -v npd-$*
	cp test/e2e-install.sh output/windows_$*/test/e2e-install.sh
	(cd output/windows_$*/ && tar -zcvf ../../$@ *)
	cp $@ $(NPD_NAME_VERSION)-windows_$*.tar.gz
	sha512sum $(NPD_NAME_VERSION)-windows_$*.tar.gz > $(NPD_NAME_VERSION)-windows_$*.tar.gz.sha512

build-binaries: $(ALL_BINARIES)

build-container: clean Dockerfile
	docker buildx create --platform $(DOCKER_PLATFORMS) --use
	docker buildx build --platform $(DOCKER_PLATFORMS) $(IMAGE_TAGS) --build-arg LOGCOUNTER=$(LOGCOUNTER) .

build-container-windows: clean Dockerfile.windows
	docker buildx create --platform windows/amd64 --use
	docker buildx build --platform windows/amd64 $(IMAGE_TAGS_WINDOWS) -f Dockerfile.windows .

$(TARBALL): ./bin/node-problem-detector ./bin/log-counter ./bin/health-checker ./test/bin/problem-maker
	tar -zcvf $(TARBALL) bin/ config/ test/e2e-install.sh test/bin/problem-maker
	sha1sum $(TARBALL)
	md5sum $(TARBALL)

build-tar: $(TARBALL) $(ALL_TARBALLS)

build: build-container build-tar

docker-builder:
	docker build -t npd-builder . --target=builder

build-in-docker: clean docker-builder
	docker run \
		-v `pwd`:/gopath/src/k8s.io/node-problem-detector/ npd-builder:latest bash \
		-c 'cd /gopath/src/k8s.io/node-problem-detector/ && make build-binaries'

push-container: build-container
	# Build should be cached from build-container
	docker buildx build --push --platform $(DOCKER_PLATFORMS) $(IMAGE_TAGS) --build-arg LOGCOUNTER=$(LOGCOUNTER) .

push-container-windows: build-container-windows
	# Build should be cached from build-container
	docker buildx build --push --platform windows/amd64 $(IMAGE_TAGS_WINDOWS) -f Dockerfile.windows .

push-tar: build-tar
	gsutil cp $(TARBALL) $(UPLOAD_PATH)/node-problem-detector/
	gsutil cp node-problem-detector-$(VERSION)-*.tar.gz* $(UPLOAD_PATH)/node-problem-detector/

# `make push` is used by presubmit and CI jobs.
push: push-container push-tar

# `make release` is used when releasing a new NPD version.
release: push-container build-tar print-tar-sha-md5

# `make release-new` is experimentally used when releasing a new NPD version.
release-new: image-$(NPD_NAME_VERSION)-linux_amd64.tar.gz image-$(NPD_NAME_VERSION)-linux_arm64.tar.gz image-$(NPD_NAME_VERSION)-windows_amd64.tar.gz print-tar-sha-md5

print-tar-sha-md5:
	./hack/print-tar-sha-md5.sh $(VERSION)

coverage.out:
	rm -f coverage.out
	go test -coverprofile=coverage.out -timeout=1m -v -short ./...

clean:
	rm -rf bin/
	rm -rf test/bin/
	rm -f node-problem-detector-*.tar.gz*
	rm -rf output/
	rm -f coverage.out

.PHONY: gomod
gomod:
	go mod tidy
	go mod vendor
	cd test; go mod tidy

.PHONY: goget
goget:
	go get $(shell go list -f '{{if not (or .Main .Indirect)}}{{.Path}}{{end}}' -mod=mod -m all)

.PHONY: depup
depup: goget gomod

# =============================================================================
# NodeMedic controller targets (Scope 2 — AFA 2026 hackathon).
#
# Spec: .specify/specs/001-nodemedic-controller/
# These targets MUST NOT touch the existing NPD targets above. They use the
# `nodemedic-` prefix and only operate on:
#   cmd/nodemedic-controller/, api/, internal/nodemedic/,
#   config/nodemedic/, deployment/helm/nodemedic-controller/,
#   test/nodemedic/, Dockerfile.nodemedic-controller
# =============================================================================

NODEMEDIC_BIN ?= bin/nodemedic-controller
NODEMEDIC_IMG ?= ghcr.io/cf/nodemedic-controller:dev
NODEMEDIC_HELM_DIR ?= deployment/helm/nodemedic-controller
NODEMEDIC_PKGS ?= ./api/... ./cmd/nodemedic-controller/... ./internal/nodemedic/...

# Pinned tool versions. Updated together when we bump controller-runtime.
CONTROLLER_TOOLS_VERSION ?= v0.16.5
ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0

# Use `go run` so we don't pollute $GOPATH/bin and pin versions per-build.
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST  ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: nodemedic-help
nodemedic-help:
	@echo "NodeMedic controller targets:"
	@echo "  nodemedic-build         build cmd/nodemedic-controller -> $(NODEMEDIC_BIN)"
	@echo "  nodemedic-generate      run controller-gen object (deepcopy)"
	@echo "  nodemedic-manifests     run controller-gen crd+rbac (writes config/nodemedic/...)"
	@echo "  nodemedic-test          go test ./api/... ./cmd/nodemedic-controller/... ./internal/nodemedic/..."
	@echo "  nodemedic-envtest       run envtest-backed reconciler tests under test/nodemedic/envtest"
	@echo "  nodemedic-docker-build  build Dockerfile.nodemedic-controller (multi-arch via buildx)"
	@echo "  nodemedic-helm-lint     helm lint $(NODEMEDIC_HELM_DIR)"
	@echo "  nodemedic-helm-package  helm package $(NODEMEDIC_HELM_DIR)"
	@echo "  nodemedic-clean         rm $(NODEMEDIC_BIN)"

.PHONY: nodemedic-build
nodemedic-build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
	  -o $(NODEMEDIC_BIN) ./cmd/nodemedic-controller

.PHONY: nodemedic-generate
nodemedic-generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: nodemedic-manifests
# `crd:allowDangerousTypes=true` is required because the contract schema
# (.specify/specs/001-nodemedic-controller/contracts/nhd-crd.yaml) uses
# `type: number` for status.diagnosis.confidence — controller-gen flags
# float64 fields as "dangerous" by default. The contract is the authority
# (Constitution Article II.2), so we opt in to numeric encoding.
#
# After generation we copy the CRD into the Helm chart's files/crd/ dir
# so `helm install` ships the same byte-for-byte CRD that controller-gen
# produced.
nodemedic-manifests:
	$(CONTROLLER_GEN) \
	  crd:allowDangerousTypes=true \
	  rbac:roleName=nodemedic-controller \
	  paths="./api/..." \
	  paths="./internal/nodemedic/..." \
	  output:crd:artifacts:config=config/nodemedic/crd \
	  output:rbac:artifacts:config=config/nodemedic/rbac
	@mkdir -p $(NODEMEDIC_HELM_DIR)/files/crd
	@cp config/nodemedic/crd/*.yaml $(NODEMEDIC_HELM_DIR)/files/crd/
	@echo "synced CRD into $(NODEMEDIC_HELM_DIR)/files/crd/"

.PHONY: nodemedic-test
nodemedic-test:
	go test -timeout=2m -count=1 $(NODEMEDIC_PKGS)

.PHONY: nodemedic-envtest
# Integration tests use envtest's binary apiserver+etcd. They live
# beside the production code under internal/nodemedic/controller/ but
# are gated by `//go:build integration` so `go test ./...` stays fast.
nodemedic-envtest:
	@echo "Setting up envtest assets for Kubernetes $(ENVTEST_K8S_VERSION)..."
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path >/dev/null
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -tags=integration -timeout=5m -count=1 ./internal/nodemedic/controller/...

.PHONY: nodemedic-docker-build
nodemedic-docker-build:
	docker buildx build \
	  -f Dockerfile.nodemedic-controller \
	  --platform linux/amd64,linux/arm64 \
	  -t $(NODEMEDIC_IMG) \
	  .

.PHONY: nodemedic-helm-lint
nodemedic-helm-lint:
	helm lint $(NODEMEDIC_HELM_DIR)
	@echo "Verifying cluster-name guard accepts cf1z (Azure kubeadm)..."
	helm template $(NODEMEDIC_HELM_DIR) --set clusterName=cf1z >/dev/null
	@echo "Verifying cluster-name guard accepts test-* (AWS/EKS)..."
	helm template $(NODEMEDIC_HELM_DIR) --set clusterName=test-odd-wire >/dev/null
	@echo "Verifying cluster-name guard rejects production-shaped names..."
	@! helm template $(NODEMEDIC_HELM_DIR) --set clusterName=stg-foo >/dev/null 2>&1 \
	  && echo "OK: helm template refused stg-foo (Constitution Article I.9)" \
	  || (echo "FAIL: helm template should reject clusterName=stg-foo per Constitution Article I.9" && exit 1)
	@! helm template $(NODEMEDIC_HELM_DIR) --set clusterName=us-big-cone >/dev/null 2>&1 \
	  && echo "OK: helm template refused us-big-cone (Constitution Article I.9)" \
	  || (echo "FAIL: helm template should reject clusterName=us-big-cone per Constitution Article I.9" && exit 1)

.PHONY: nodemedic-helm-package
nodemedic-helm-package:
	helm package $(NODEMEDIC_HELM_DIR) -d $(NODEMEDIC_HELM_DIR)/..

.PHONY: nodemedic-clean
nodemedic-clean:
	rm -f $(NODEMEDIC_BIN)

# ===========================================================================
# NodeMedic AGENT (Scope 3 — AFA 2026 hackathon)
# Spec: .specify/specs/002-nodemedic-agent/
#
# All targets are namespaced with `nodemedic-agent-` and operate only on:
#   cmd/nodemedic-agent/, nodemedic_agent/, prompts/,
#   deployment/helm/nodemedic-agent/, tests/nodemedic_agent/,
#   Dockerfile.nodemedic-agent, pyproject.toml, uv.lock
# NPD's existing targets and the controller's `nodemedic-*` targets are
# unchanged.
# ===========================================================================

NODEMEDIC_AGENT_IMG ?= cf-registry.nr-ops.net/container-fabric/nodemedic-agent
NODEMEDIC_AGENT_HELM_DIR ?= deployment/helm/nodemedic-agent
NODEMEDIC_AGENT_TESTS_DIR ?= tests/nodemedic_agent

# TAG defaults to dev-cf1z-<shortsha>; override with `make … TAG=…`.
NODEMEDIC_AGENT_TAG ?= dev-cf1z-$(shell git rev-parse --short=8 HEAD 2>/dev/null || echo unknown)

.PHONY: nodemedic-agent-help
nodemedic-agent-help:
	@echo "NodeMedic agent (Scope 3) make targets:"
	@echo "  nodemedic-agent-test          uv run pytest tests/nodemedic_agent/"
	@echo "  nodemedic-agent-lint          ruff/format if added; placeholder today"
	@echo "  nodemedic-agent-runbook-lint  bash $(NODEMEDIC_AGENT_TESTS_DIR)/check_runbook.sh prompts/runbook.md"
	@echo "  nodemedic-agent-helm-lint     helm lint $(NODEMEDIC_AGENT_HELM_DIR)"
	@echo "  nodemedic-agent-docker-build  docker buildx build -f Dockerfile.nodemedic-agent (linux/amd64)"
	@echo "  nodemedic-agent-docker-push   docker push $(NODEMEDIC_AGENT_IMG):$(NODEMEDIC_AGENT_TAG)"
	@echo "  nodemedic-agent-clean         rm -rf .venv .pytest_cache __pycache__"

.PHONY: nodemedic-agent-test
nodemedic-agent-test:
	uv run pytest $(NODEMEDIC_AGENT_TESTS_DIR)/ -v

.PHONY: nodemedic-agent-lint
nodemedic-agent-lint:
	@echo "nodemedic-agent-lint: no linter wired in v1 (deferred to Phase 8 polish)"

.PHONY: nodemedic-agent-runbook-lint
nodemedic-agent-runbook-lint:
	bash $(NODEMEDIC_AGENT_TESTS_DIR)/check_runbook.sh prompts/runbook.md

.PHONY: nodemedic-agent-helm-lint
nodemedic-agent-helm-lint:
	helm lint $(NODEMEDIC_AGENT_HELM_DIR) --set clusterName=cf1z

.PHONY: nodemedic-agent-docker-build
nodemedic-agent-docker-build:
	docker buildx build \
	  -f Dockerfile.nodemedic-agent \
	  --platform linux/amd64 \
	  -t $(NODEMEDIC_AGENT_IMG):$(NODEMEDIC_AGENT_TAG) \
	  --load \
	  .

.PHONY: nodemedic-agent-docker-push
nodemedic-agent-docker-push:
	docker push $(NODEMEDIC_AGENT_IMG):$(NODEMEDIC_AGENT_TAG)

.PHONY: nodemedic-agent-clean
nodemedic-agent-clean:
	rm -rf .venv .pytest_cache .ruff_cache .mypy_cache
	find nodemedic_agent tests/nodemedic_agent -type d -name __pycache__ -prune -exec rm -rf {} +
