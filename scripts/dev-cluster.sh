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

# The in-cluster git server. Credentials are fixed and public on purpose: this
# cluster is disposable, offline, and holds nothing worth protecting. The e2e
# suite reads them from here.
GIT_USER="hecate"
GIT_PASSWORD="hecate-dev"
GIT_REPO="fleet"
GIT_LOCAL_PORT="${GIT_LOCAL_PORT:-13000}"
GIT_URL="http://gitea.hecate-git.svc.cluster.local:3000/${GIT_USER}/${GIT_REPO}.git"

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
    # K3S_IMAGE pins the Kubernetes version. Unset means k3d's default, which
    # is right for local work; CI sets it so the three e2e legs between them
    # span the Kubernetes range we claim to support (#88).
    #
    # HECATE_OIDC=1 additionally configures kube-apiserver to trust the Dex
    # this repo can install, which is the only way browser sign-in can be
    # exercised locally. It has to happen here: kube-apiserver reads those
    # flags at startup, so it cannot be added to a cluster that already exists.
    local oidc_args=()
    if [ -n "${HECATE_OIDC:-}" ]; then
      log "configuring the API server to trust the development Dex"
      # mapfile, not $(...) word-splitting: the flags contain `*` and would be
      # globbed against the working directory.
      mapfile -t oidc_args < <("$(dirname "$0")/oidc.sh" args)
    fi

    k3d cluster create "$CLUSTER" \
      --agents 1 \
      ${K3S_IMAGE:+--image "$K3S_IMAGE"} \
      "${oidc_args[@]}" \
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

  git_server

  if [ -n "${HECATE_OIDC:-}" ]; then
    "$(dirname "$0")/oidc.sh" install
  fi

  log "installing Hecate CRDs"
  # Server-side apply: a later `helm install` also applies these, and
  # client-side apply leaves field ownership that Helm then conflicts with.
  kubectl apply --server-side --force-conflicts -f charts/hecate/crds/ >/dev/null

  cat <<EOF

Cluster $CLUSTER is ready.

  export KUBECONFIG=$KUBECONFIG_PATH
  kubectl get pods -n flux-system
  kubectl get beacons,bundles,gates,passages -A

  local registry: localhost:${REGISTRY_PORT}  (in-cluster: ${REGISTRY}:${REGISTRY_PORT})
  git server:     ${GIT_URL}
                  user ${GIT_USER}, password ${GIT_PASSWORD}
EOF
}

# git_server installs Gitea and seeds the repository the e2e promotion writes to.
#
# It exists because a promotion is a git write, and until there was a git host
# in the cluster the deployed controller had never performed one. See
# scripts/git-server.yaml for why Gitea rather than something smaller.
git_server() {
  # Seeding talks to Gitea's API rather than cloning, so it needs these two.
  # Named here rather than at the top: nothing else in the script uses them, and
  # a missing tool should say which step wanted it.
  require curl base64

  if kubectl get namespace hecate-git >/dev/null 2>&1; then
    log "git server already installed"
    return
  fi

  log "installing the git server"
  kubectl apply -f "$(dirname "$0")/git-server.yaml" >/dev/null
  kubectl -n hecate-git rollout status deploy/gitea --timeout=180s >/dev/null

  local pod
  pod=$(kubectl -n hecate-git get pod -l app=gitea -o jsonpath='{.items[0].metadata.name}')
  # Gitea refuses to run its CLI as root, and kubectl exec lands there.
  kubectl -n hecate-git exec "$pod" -- su git -c \
    "gitea admin user create --username ${GIT_USER} --password ${GIT_PASSWORD} \
       --email ${GIT_USER}@hecate.test --admin --must-change-password=false" >/dev/null

  seed_repo
  log "git server ready at ${GIT_URL}"
}

# seed_repo creates the fleet repository and the manifests a promotion edits.
#
# Written through Gitea's contents API rather than by cloning and pushing: the
# script would otherwise need a git client and a working tree, and the API does
# it in one call per file.
seed_repo() {
  local pf_pid api="http://127.0.0.1:${GIT_LOCAL_PORT}/api/v1"
  kubectl -n hecate-git port-forward svc/gitea "${GIT_LOCAL_PORT}:3000" >/dev/null 2>&1 &
  pf_pid=$!
  # shellcheck disable=SC2064
  trap "kill $pf_pid 2>/dev/null || true" RETURN

  local waited=0
  until curl -sf -o /dev/null "http://127.0.0.1:${GIT_LOCAL_PORT}/api/healthz"; do
    sleep 1
    waited=$((waited + 1))
    [ "$waited" -lt 60 ] || die "the git server did not become reachable"
  done

  curl -sf -u "${GIT_USER}:${GIT_PASSWORD}" -X POST "${api}/user/repos" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${GIT_REPO}\",\"auto_init\":true,\"default_branch\":\"main\"}" >/dev/null

  put_file "$api" "apps/staging/configmap.yaml" "$(cat <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-git-payload
data:
  # A promotion rewrites this, so the applied state visibly follows the commit.
  tag: "0.0.0"
YAML
)"

  put_file "$api" "apps/staging/kustomization.yaml" "$(cat <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
images:
  - name: ghcr.io/stefanprodan/podinfo
    newTag: "0.0.0"
YAML
)"
}

# put_file writes one file into the seeded repository.
put_file() {
  local api="$1" path="$2" content="$3"
  curl -sf -u "${GIT_USER}:${GIT_PASSWORD}" -X POST "${api}/repos/${GIT_USER}/${GIT_REPO}/contents/${path}" \
    -H "Content-Type: application/json" \
    -d "$(printf '{"branch":"main","message":"seed %s","content":"%s"}' \
          "$path" "$(printf '%s' "$content" | base64 -w0)")" >/dev/null \
    || die "seeding $path failed"
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
  # A unique tag per build by default: sharing one tag between builds leaves
  # helm with an unchanged Deployment and the cluster running the old binary.
  local tag="localhost:${REGISTRY_PORT}/hecate-controller:${HECATE_DEV_TAG:-dev-$(date +%s)}"
  log "building $tag"
  docker build -t "$tag" -f Dockerfile .
  docker push "$tag"
  log "pushed — reference it in-cluster as ${REGISTRY}:${REGISTRY_PORT}/hecate-controller:${HECATE_DEV_TAG:-dev}"
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
