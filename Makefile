# Hecate — development tasks.
#
# Everything here assumes the Nix dev shell (`nix develop`, or direnv). The
# tools are pinned there, not installed globally, so a clean checkout builds
# the same way on every machine.

CONTROLLER_GEN ?= controller-gen
CHART_DIR      := charts/hecate

# A unique tag per build, evaluated once per make run.
#
# Reusing one `dev` tag meant `helm upgrade` saw an unchanged Deployment and
# never restarted the pod, so the cluster kept running the previous binary
# while the build appeared to succeed. imagePullPolicy=Always does not help:
# it only applies when a pod is created. Two different builds are two different
# images, and giving them the same tag was the mistake.
DEV_TAG        := dev-$(shell date +%s)

# The dev shell sets this; default it so the cluster targets also work from a
# bare shell and from CI, and never touch a real cluster by accident.
export KUBECONFIG ?= $(CURDIR)/.dev/kubeconfig

.DEFAULT_GOAL := help
.PHONY: help test vet fmt lint check flake-hash generate build run cluster cluster-rm cluster-load install uninstall e2e secrets-edit secrets-rekey clean

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

check: vet test flake-hash-check ## Everything CI enforces, locally
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

flake-hash-check: ## Fail if flake.nix's vendorHash is stale
	@# This has broken the build three times, and each time the reasoning that
	@# let it through was "go.mod did not change, so the hash cannot have".
	@# That is wrong: vendorHash covers the set of modules actually downloaded,
	@# which follows the *import graph*. Importing a new package from a module
	@# already in go.mod — k8s.io/client-go/util/retry, say — moves it while
	@# go.mod and go.sum stay byte-identical.
	@#
	@# A plain `nix build` will not catch it either: a fixed-output derivation
	@# whose hash is already in the store is treated as realised, so a stale
	@# hash validates against a leftover build and only fails in CI.
	@computed=$$($(MAKE) --no-print-directory flake-hash 2>/dev/null | grep -o 'sha256-[^"]*'); \
	 declared=$$(grep -o 'sha256-[^"]*' flake.nix); \
	 if [ "$$computed" != "$$declared" ]; then \
	   echo "flake.nix vendorHash is stale."; \
	   echo "  declared: $$declared"; \
	   echo "  computed: $$computed"; \
	   echo "Fix: set vendorHash in flake.nix to the computed value."; \
	   exit 1; \
	 fi

flake-hash: ## Print the vendorHash flake.nix needs (run after adding a Go dependency)
	@# A stale vendorHash cannot be detected by a plain `nix build`: Nix treats a
	@# fixed-output derivation as already realised when an output with the
	@# specified hash is in the store, so it validates against a leftover build.
	@# Forcing a bogus hash makes it recompute and report the real one.
	@sed 's|vendorHash = "[^"]*"|vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="|' \
		flake.nix > /tmp/hecate-flake-probe.nix
	@cp flake.nix /tmp/hecate-flake-real.nix && cp /tmp/hecate-flake-probe.nix flake.nix; \
		out=$$(nix build .#default --no-link 2>&1 | grep -oP 'got: *\K\S+' | head -1); \
		cp /tmp/hecate-flake-real.nix flake.nix; \
		if [ -n "$$out" ]; then echo "vendorHash = \"$$out\";"; \
		else echo "vendorHash is already correct"; fi

generate: ## Regenerate deepcopy, CRDs and RBAC from the API and controller markers
	$(CONTROLLER_GEN) \
		object:headerFile=/dev/null paths=./api/... \
		crd output:crd:dir=$(CHART_DIR)/crds \
		rbac:roleName=hecate-controller output:rbac:dir=$(CHART_DIR)/rbac \
		paths=./pkg/...

build: ## Build the controller and the CLI
	go build -o bin/hecate-controller ./cmd/hecate-controller
	go build -o bin/hecate ./cmd/hecate
	go build -o bin/hecate-mcp ./cmd/hecate-mcp

run: ## Run the controller against the current KUBECONFIG
	go run ./cmd/hecate-controller --zap-devel

## ------------------------------------------------------------- cluster ----

cluster: ## Create the k3d dev cluster in Docker and install Flux
	./scripts/dev-cluster.sh up

cluster-rm: ## Delete the k3d dev cluster
	./scripts/dev-cluster.sh down

cluster-load: ## Build the controller image and push it to the cluster registry
	HECATE_DEV_TAG=$(DEV_TAG) ./scripts/dev-cluster.sh load

install: cluster-load ## Build, push and install the chart into the dev cluster
	helm upgrade --install hecate $(CHART_DIR) \
		--namespace hecate-system --create-namespace \
		--set image.repository=hecate-registry:5001/hecate-controller \
		--set image.tag=$(DEV_TAG) --set image.pullPolicy=Always \
		--wait --timeout 3m

uninstall: ## Remove the chart from the dev cluster (CRDs are left alone)
	helm uninstall hecate --namespace hecate-system || true

e2e: ## Run the end-to-end suite against the dev cluster
	@./scripts/dev-cluster.sh status >/dev/null || { echo "no dev cluster — run 'make cluster'"; exit 1; }
	go test -tags e2e -count=1 -timeout 20m ./test/e2e/...

fides-test: ## Check the Fides client against a real server (FIDES_SERVER_URL, FIDES_TOKEN)
	@test -n "$$FIDES_SERVER_URL" || { echo "set FIDES_SERVER_URL (see docs/ONBOARDING.md)"; exit 1; }
	@test -n "$$FIDES_TOKEN" || { echo "set FIDES_TOKEN"; exit 1; }
	go test -tags fides -count=1 -v ./test/integration/...

## ------------------------------------------------------------- secrets ----

secrets-edit: ## Edit an encrypted secret: make secrets-edit SECRET=github-token.age
	@test -n "$(SECRET)" || { echo "usage: make secrets-edit SECRET=<name>.age"; exit 1; }
	cd secrets && agenix -e $(SECRET)

secrets-rekey: ## Re-encrypt every secret after changing recipients
	cd secrets && agenix -r

clean: ## Remove build output and the local kubeconfig
	rm -rf bin .dev
