# Hecate — development tasks.
#
# Everything here assumes the Nix dev shell (`nix develop`, or direnv). The
# tools are pinned there, not installed globally, so a clean checkout builds
# the same way on every machine.

CONTROLLER_GEN ?= controller-gen
CHART_DIR      := charts/hecate

.DEFAULT_GOAL := help
.PHONY: help test vet fmt lint check generate build run cluster cluster-rm cluster-load install uninstall e2e secrets-edit secrets-rekey clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1;33m%-14s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- code ----

test: ## Run the full suite (no cluster required — that is the bar)
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## Format Go and Nix
	gofmt -w .
	nixfmt *.nix

lint: ## Lint Go and Nix
	golangci-lint run
	statix check .
	deadnix --fail .

check: vet test ## Everything CI enforces, locally
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

generate: ## Regenerate deepcopy, CRDs and RBAC from the API and controller markers
	$(CONTROLLER_GEN) \
		object:headerFile=/dev/null paths=./api/... \
		crd output:crd:dir=$(CHART_DIR)/crds \
		rbac:roleName=hecate-controller output:rbac:dir=$(CHART_DIR)/rbac \
		paths=./pkg/...

build: ## Build the controller binary
	go build -o bin/hecate-controller ./cmd/hecate-controller

run: ## Run the controller against the current KUBECONFIG
	go run ./cmd/hecate-controller --zap-devel

## ------------------------------------------------------------- cluster ----

cluster: ## Create the k3d dev cluster in Docker and install Flux
	./scripts/dev-cluster.sh up

cluster-rm: ## Delete the k3d dev cluster
	./scripts/dev-cluster.sh down

cluster-load: ## Build the controller image and push it to the cluster registry
	./scripts/dev-cluster.sh load

install: cluster-load ## Build, push and install the chart into the dev cluster
	helm upgrade --install hecate $(CHART_DIR) \
		--namespace hecate-system --create-namespace \
		--set image.repository=hecate-registry:5001/hecate-controller \
		--set image.tag=dev --set image.pullPolicy=Always \
		--wait --timeout 3m

uninstall: ## Remove the chart from the dev cluster (CRDs are left alone)
	helm uninstall hecate --namespace hecate-system || true

e2e: ## Run the end-to-end suite against the dev cluster
	@./scripts/dev-cluster.sh status >/dev/null || { echo "no dev cluster — run 'make cluster'"; exit 1; }
	go test -tags e2e -count=1 -timeout 20m ./test/e2e/...

## ------------------------------------------------------------- secrets ----

secrets-edit: ## Edit an encrypted secret: make secrets-edit SECRET=github-token.age
	@test -n "$(SECRET)" || { echo "usage: make secrets-edit SECRET=<name>.age"; exit 1; }
	cd secrets && agenix -e $(SECRET)

secrets-rekey: ## Re-encrypt every secret after changing recipients
	cd secrets && agenix -r

clean: ## Remove build output and the local kubeconfig
	rm -rf bin .dev
