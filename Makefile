# Kybernate build targets. Tools are always used in their latest release.

IMAGE_REGISTRY ?= ghcr.io/kybernate
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PLATFORMS      ?= linux/amd64,linux/arm64

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest
HELM           ?= helm
# go-nvml's NVML header marks many functions deprecated; keep build logs readable.
export CGO_CFLAGS ?= -O2 -g -Wno-deprecated-declarations

LDFLAGS        := -s -w -X github.com/kybernate/kybernate/internal/version.Version=$(VERSION) -X github.com/kybernate/kybernate/internal/version.Commit=$(COMMIT)

.PHONY: all
all: verify vet test build

.PHONY: generate
generate: ## Generate the DeepCopy methods of the API types.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: ## Generate the CRDs and copy them into the Helm chart.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	mkdir -p charts/kybernate/files/crds
	cp config/crd/bases/*.yaml charts/kybernate/files/crds/

.PHONY: verify
verify: generate manifests ## Fail if generated files are out of date.
	git diff --exit-code -- api config charts/kybernate/files/crds

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: build
build: ## Build all binaries into ./bin.
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/node-agent ./cmd/node-agent

.PHONY: image
image: ## Build the node-agent image for the local platform.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_REGISTRY)/node-agent:$(VERSION) .

.PHONY: image-multiarch
image-multiarch: ## Build the node-agent image for all platforms.
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_REGISTRY)/node-agent:$(VERSION) .

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart.
	$(HELM) lint charts/kybernate
	$(HELM) template kybernate charts/kybernate > /dev/null

.PHONY: clean
clean:
	rm -rf bin
