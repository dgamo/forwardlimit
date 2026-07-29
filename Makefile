BINARY  := forwardlimit
PKG     := ./...
GO      ?= go

# The limiter file `make run` uses. Override it: make run CONFIG=path/to/config.yaml
CONFIG  ?= examples/config/simple.yaml
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Tool versions are pinned here rather than in CI, so a local run and the pipeline
# are the same code path. The one lint failure that ever reached CI on this project
# happened because they were not.
GOLANGCI_VERSION   ?= v2.1.6
KUBECONFORM_VERSION ?= v0.7.0
GOBIN               := $(shell $(GO) env GOPATH)/bin
GOLANGCI            := $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)
KUBECONFORM         := $(shell command -v kubeconform 2>/dev/null || echo $(GOBIN)/kubeconform)

# Where the end-to-end suite points. Override to smoke-test a real deployment.
E2E_DIR := $(CURDIR)/examples/docker-compose

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -l -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: tools
tools: ## Install the pinned dev tools into $(GOBIN)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	$(GO) install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

.PHONY: lint
lint: fmt-check vet ## Run gofmt, go vet and golangci-lint
	@if [ -x "$(GOLANGCI)" ]; then \
		"$(GOLANGCI)" run; \
	else \
		echo "ERROR: golangci-lint not found at $(GOLANGCI)."; \
		echo "       Refusing to report success from a partial lint - CI runs revive,"; \
		echo "       staticcheck, gosec and others that gofmt and vet do not cover."; \
		echo "       Install the pinned version with:  make tools"; \
		exit 1; \
	fi

.PHONY: test
test: ## Run unit tests (skips integration tests)
	$(GO) test -short $(PKG)

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	$(GO) test -race -short $(PKG)

.PHONY: test-integration
test-integration: ## Run integration tests (needs a reachable Redis)
	FORWARDLIMIT_INTEGRATION=1 $(GO) test -count=1 -run Integration $(PKG)

.PHONY: cover
cover: ## Run tests with coverage summary
	$(GO) test -short -covermode=atomic -coverprofile=coverage.out $(PKG)
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: build
build: ## Build the binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/$(BINARY) ./cmd/forwardlimit

.PHONY: docker
docker: ## Build the container image locally
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

.PHONY: run
run: build ## Build and run locally against CONFIG
	./bin/$(BINARY) -config $(CONFIG)

.PHONY: validate
validate: build ## Check every example configuration
	@for f in examples/config/*.yaml examples/docker-compose/forwardlimit.yaml; do \
		HASH_SECRET=$${HASH_SECRET:-dev-secret} ./bin/$(BINARY) -validate -config "$$f" || exit 1; \
	done

.PHONY: manifests
manifests: ## Schema-check the Kubernetes examples
	@if [ ! -x "$(KUBECONFORM)" ]; then \
		echo "ERROR: kubeconform not found at $(KUBECONFORM). Install it with: make tools"; \
		exit 1; \
	fi
	@# No -ignore-missing-schemas: with it, a typo'd `kind` is silently SKIPPED and
	@# the check still passes. The CRDs-catalog below covers Traefik's Middleware, so
	@# every kind here resolves and an unknown one is a hard error - which is the
	@# whole point of running this.
	@"$(KUBECONFORM)" -strict -summary \
		-schema-location default \
		-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
		examples/kubernetes/sidecar/manifests.yaml \
		examples/kubernetes/standalone/manifests.yaml

.PHONY: e2e
e2e: ## Bring the Compose example up, run the e2e suite against it, tear it down
	@set -e; \
	cd $(E2E_DIR) && docker compose up -d --build --wait; \
	cd $(CURDIR); \
	status=0; \
	FORWARDLIMIT_E2E=1 FORWARDLIMIT_E2E_COMPOSE=$(E2E_DIR) \
		$(GO) test -count=1 -timeout 10m ./e2e/... || status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo "--- container logs ---"; \
		(cd $(E2E_DIR) && docker compose logs --no-color --tail=150) || true; \
	fi; \
	(cd $(E2E_DIR) && docker compose down -v) || true; \
	exit $$status

.PHONY: ci
ci: lint cover build validate manifests ## Everything the pipeline checks, bar e2e

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out
