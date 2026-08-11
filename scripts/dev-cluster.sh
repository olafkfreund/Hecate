#!/usr/bin/env bash
# Manage the local k3d cluster used for development and e2e.
#
# k3d runs k3s inside Docker: a real API server, real Flux controllers, real
# reconciliation — but disposable and offline. Everything Hecate does that
# cannot be unit-tested (reading actual Flux status output) needs a cluster
# this cheap, or it does not get tested at all.
set -euo pipefail

CLUSTER="${HECATE_DEV_CLUSTER:-hecate-dev}"
REGISTRY="hecate-registry"
REGISTRY_PORT="5001"
KUBECONFIG_PATH="${KUBECONFIG:-$PWD/.dev/kubeconfig}"
FLUX_VERSION="${FLUX_VERSION:-}"

log() { printf '\033[1;33m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "$cmd is not on PATH — are you in \`nix develop\`?"
  done
}

up() {
  require k3d kubectl flux
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

  if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
    log "cluster $CLUSTER already exists"
  else
    log "creating k3d cluster $CLUSTER with a local registry on :$REGISTRY_PORT"
    # A local registry means the dev image never leaves the machine, so the
    # inner loop does not depend on GHCR or on being online.
    k3d cluster create "$CLUSTER" \
      --agents 1 \
      --registry-create "${REGISTRY}:0.0.0.0:${REGISTRY_PORT}" \
      --wait
  fi

  mkdir -p "$(dirname "$KUBECONFIG_PATH")"
  k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG_PATH"
  chmod 600 "$KUBECONFIG_PATH"
  export KUBECONFIG="$KUBECONFIG_PATH"

  log "waiting for the API server"
  kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null

  if kubectl get namespace flux-system >/dev/null 2>&1; then
    log "Flux already installed"
  else
    log "installing Flux${FLUX_VERSION:+ $FLUX_VERSION}"
    # `flux install` rather than `flux bootstrap`: bootstrap wants a git
    # provider and a token, which a local loop should not require.
    flux install ${FLUX_VERSION:+--version="$FLUX_VERSION"} --components-extra=image-reflector-controller,image-automation-controller
  fi

  log "installing Hecate CRDs"
  kubectl apply -f charts/hecate/crds/ >/dev/null

  cat <<EOF

Cluster $CLUSTER is ready.

  export KUBECONFIG=$KUBECONFIG_PATH
  kubectl get pods -n flux-system
  kubectl get beacons,bundles,gates,passages -A

  local registry: localhost:${REGISTRY_PORT}  (in-cluster: ${REGISTRY}:${REGISTRY_PORT})
EOF
}

down() {
  require k3d
  if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
    log "deleting cluster $CLUSTER"
    k3d cluster delete "$CLUSTER"
  else
    log "cluster $CLUSTER does not exist"
  fi
  rm -f "$KUBECONFIG_PATH"
}

# load builds the controller image and pushes it to the cluster's registry, so
# a code change reaches a running cluster without a container registry account.
load() {
  require docker k3d
  local tag="localhost:${REGISTRY_PORT}/hecate-controller:dev"
  log "building $tag"
  docker build -t "$tag" -f Dockerfile .
  docker push "$tag"
  log "pushed — reference it in-cluster as ${REGISTRY}:${REGISTRY_PORT}/hecate-controller:dev"
}

status() {
  require k3d
  k3d cluster list "$CLUSTER" 2>/dev/null || { echo "cluster $CLUSTER does not exist"; return 1; }
}

case "${1:-}" in
  up)     up ;;
  down)   down ;;
  load)   load ;;
  status) status ;;
  *)      die "usage: $0 {up|down|load|status}" ;;
esac
