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
.PHONY: help test vet fmt lint check flake-hash generate build run ui ui-test ui-dev oidc-check cluster cluster-rm cluster-load install uninstall collector e2e secrets-edit secrets-rekey clean

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

lint: ## Lint Go, Nix and the UI
	@# golangci-lint's defaults do not include gofmt, and CI checks it in a
	@# separate step — so `make lint` passed while CI failed on formatting.
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	golangci-lint run
	statix check .
	deadnix --fail .
	@# The UI too, and it belongs here for the reason stated above rather than a
	@# different one: CI lints it in its own job, so `make lint` passing said
	@# nothing about whether the push would. A React hooks violation in the
	@# settings page got through exactly that gap.
	@# Skipped when the app has never been installed, so `make lint` still works
	@# for someone touching only Go and without Node.
	@if [ -d ui/node_modules ]; then cd ui && npm run --silent lint; \
	else echo "ui: skipped (run 'cd ui && npm ci' to lint it)"; fi

check: vet test flake-hash-check crd-embed-check ## Everything CI enforces, locally
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

crd-embed-check: ## Fail if the embedded CRDs have drifted from the chart's
	@# Two copies of a generated file is a thing that drifts. If it did, the
	@# controller would check the cluster against CRDs nobody installs, which is
	@# worse than not checking at all — it would pass while the real ones were stale.
	@for crd in $(CHART_DIR)/crds/*.yaml; do \
	  if ! cmp -s "$$crd" "pkg/crds/$$(basename $$crd)"; then \
	    echo "pkg/crds/$$(basename $$crd) differs from the chart's. Run 'make generate'."; \
	    exit 1; \
	  fi; \
	done

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
	 if [ -z "$$computed" ]; then \
	   echo "could not compute the vendorHash."; \
	   echo "Most likely a new file is untracked: a flake only sees what git does."; \
	   echo "Try 'git add -A' and run again."; \
	   exit 1; \
	 fi; \
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
	@# The controller embeds the CRDs so it can refuse to run against an older
	@# API (#117), and go:embed cannot reach outside its own package. Copied
	@# rather than symlinked because embed does not follow symlinks either.
	cp $(CHART_DIR)/crds/*.yaml pkg/crds/
	@# The field names the API sends, so ui/lib/api.ts can be checked against
	@# them rather than trusted. That file mirrors these types by hand and has
	@# drifted once already, silently, because nothing validates a response
	@# against a schema in the browser.
	go run ./cmd/apishape > ui/test/apishape.json
	@# A JSON Schema per step, generated from the same config structs CheckConfig
	@# decodes into (#114). Committed rather than reflected at runtime: the field
	@# descriptions are the Go doc comments, and reading those needs the source.
	go run ./cmd/stepschema > pkg/passage/steps/schemas.json

build: ## Build the controller and the CLI
	go build -o bin/hecate-controller ./cmd/hecate-controller
	go build -o bin/hecate ./cmd/hecate
	go build -o bin/hecate-mcp ./cmd/hecate-mcp
	go build -o bin/hecate-api ./cmd/hecate-api

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
	@# Helm installs crds/ once and never touches it again on upgrade, so a new
	@# API field would be silently pruned by the API server and the controller
	@# would see a zero value. That cost an hour: watch[].image.insecure was set
	@# in the object, absent from the cluster's CRD, and the Beacon kept trying
	@# HTTPS. Applied explicitly here, and server-side so Helm's field ownership
	@# does not conflict.
	kubectl apply --server-side --force-conflicts -f $(CHART_DIR)/crds/
	helm upgrade --install hecate $(CHART_DIR) \
		--namespace hecate-system --create-namespace \
		--set image.repository=hecate-registry:5001/hecate-controller \
		--set image.tag=$(DEV_TAG) --set image.pullPolicy=Always \
		--wait --timeout 3m

uninstall: ## Remove the chart from the dev cluster (CRDs are left alone)
	helm uninstall hecate --namespace hecate-system || true

ui: ## Build the web UI into the API binary
	@# --include=dev explicitly: NODE_ENV=production makes npm omit
	@# devDependencies, and the build needs TypeScript and Tailwind from there.
	cd ui && npm ci --include=dev --no-audit --no-fund
	cd ui && npm run build
	@# Replaced wholesale rather than merged, so a file deleted from the app
	@# does not linger in the binary. Safe to wipe: the placeholder lives in
	@# pkg/api/, not in here.
	rm -rf pkg/api/ui && mkdir -p pkg/api/ui
	touch pkg/api/ui/.gitkeep
	cp -r ui/out/. pkg/api/ui/
	@echo "UI built into pkg/api/ui — rebuild hecate-api to pick it up"

ui-test: ## Run the UI's tests
	cd ui && npm test

ui-dev: ## Run the UI against a local hecate-api on :8080
	cd ui && npm run dev

oidc-check: ## Prove browser sign-in works, by doing it
	./scripts/oidc.sh check

collector: ## Deploy a span-printing OTel collector into the dev cluster
	kubectl apply -f dev/collector.yaml
	kubectl rollout status deploy/otelcol -n hecate-system --timeout=120s
	@echo "Spans: kubectl logs deploy/otelcol -n hecate-system -f"

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
