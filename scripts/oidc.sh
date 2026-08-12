#!/usr/bin/env bash
# Stand up Dex in the dev cluster, so browser sign-in can be exercised.
#
# The hard part of a local OIDC setup is that the issuer URL has to resolve to
# the same thing from three places: the browser on this machine, hecate-api on
# this machine, and kube-apiserver inside the k3d server container. The usual
# answer is an /etc/hosts entry, which a setup script should not be writing on
# someone's machine. This uses nip.io instead — dex.<host-ip>.nip.io resolves to
# <host-ip> for everyone, publicly, with no local configuration at all.
#
# Everything here is disposable: a self-signed CA, one static password, a client
# secret in a ConfigMap. The cluster is offline and holds nothing.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="${HECATE_OIDC_DIR:-$PWD/.dev/oidc}"

# Both to stderr: `args` writes k3d flags to stdout and dev-cluster.sh reads
# them, so a progress line on stdout becomes a positional argument to k3d.
log() { printf '\033[1;33m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# host_ip is an address the browser, this host and the cluster containers can
# all reach. The default route's source address is the one that satisfies all
# three; loopback satisfies only the first two.
host_ip() {
  ip route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}'
}

# certs generates a CA and a server certificate for the issuer hostname.
#
# Regenerated only when missing: the CA is mounted into the k3d node at cluster
# creation, so changing it means recreating the cluster, and silently doing that
# on every run would be a surprising thing for a script to do.
certs() {
  local host="$1"
  mkdir -p "$OUT"
  if [ -f "$OUT/ca.crt" ] && [ -f "$OUT/tls.crt" ]; then
    log "reusing the certificates in $OUT"
    return
  fi

  log "generating a CA and a certificate for $host"
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout "$OUT/ca.key" -out "$OUT/ca.crt" \
    -subj "/CN=hecate-dev-ca" >/dev/null 2>&1

  openssl req -newkey rsa:2048 -nodes \
    -keyout "$OUT/tls.key" -out "$OUT/tls.csr" \
    -subj "/CN=$host" >/dev/null 2>&1

  # A SAN is not optional: Go has rejected certificates without one since 1.15,
  # and both kube-apiserver and hecate-api are Go.
  printf 'subjectAltName=DNS:%s\n' "$host" > "$OUT/san.cnf"
  openssl x509 -req -in "$OUT/tls.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" \
    -CAcreateserial -out "$OUT/tls.crt" -days 3650 -extfile "$OUT/san.cnf" >/dev/null 2>&1

  chmod 600 "$OUT/ca.key" "$OUT/tls.key"
}

# args prints the k3d flags that make the cluster trust Dex. dev-cluster.sh
# reads these when creating the cluster; they cannot be added afterwards,
# because kube-apiserver takes them at startup.
args() {
  local host issuer
  host="dex.$(host_ip).nip.io"
  issuer="https://$host:5556/dex"
  certs "$host"

  # One argument per line. The caller reads them with mapfile rather than
  # word-splitting, because these contain `*` and `@` — splitting would glob
  # `@server:*` against the working directory, which is a mistake that only
  # shows up on a machine where such a file happens to exist.
  cat <<ARGS
--port
5556:5556@loadbalancer
--volume
$OUT/ca.crt:/etc/ssl/dex-ca.crt@server:*
--k3s-arg
--kube-apiserver-arg=oidc-issuer-url=$issuer@server:*
--k3s-arg
--kube-apiserver-arg=oidc-client-id=hecate@server:*
--k3s-arg
--kube-apiserver-arg=oidc-ca-file=/etc/ssl/dex-ca.crt@server:*
--k3s-arg
--kube-apiserver-arg=oidc-username-claim=email@server:*
--k3s-arg
--kube-apiserver-arg=oidc-groups-claim=groups@server:*
ARGS
}

# install deploys Dex and grants the static user something to do.
install() {
  command -v openssl >/dev/null || die "openssl is not on PATH"
  local host issuer sum
  host="dex.$(host_ip).nip.io"
  issuer="https://$host:5556/dex"
  certs "$host"

  log "installing Dex at $issuer"

  # bcrypt of "hecate-dev", generated and verified rather than typed. The first
  # version of this line was a hash I asserted was bcrypt of that password and
  # never checked; it was not, so Dex rejected every login with "Invalid Email
  # Address and password" — after the whole cluster had been rebuilt around it.
  #
  # Hard-coded rather than generated at install time because a fixed hash for a
  # fixed throwaway password in an offline cluster is not a secret, and
  # generating it would need a bcrypt implementation the dev shell does not
  # otherwise carry. `make oidc-hash` regenerates it if the password changes.
  local hash='$2a$10$Wu3phLaPTxK.W.4Y777Erut8L1cL7mVo.s.Q1a7njRSA976m8ilSm'

  # Over the rendered config, so a changed issuer or password actually
  # restarts Dex. See the annotation's comment in dex.yaml.tmpl.
  sum=$(printf '%s%s' "$issuer" "$hash" | sha256sum | cut -c1-16)
  sed -e "s|ISSUER|$issuer|g" \
      -e "s|BCRYPT_HASH|$hash|g" \
      -e "s|CONFIG_SUM|$sum|g" \
      -e "s|TLS_CRT|$(base64 -w0 < "$OUT/tls.crt")|g" \
      -e "s|TLS_KEY|$(base64 -w0 < "$OUT/tls.key")|g" \
      "$DIR/dex.yaml.tmpl" | kubectl apply -f - >/dev/null

  kubectl -n hecate-auth rollout status deploy/dex --timeout=120s >/dev/null

  # Without this the login succeeds and every request afterwards is refused,
  # which is exactly the failure the flag's help text warns about — worth
  # demonstrating the fix rather than the symptom.
  kubectl create clusterrolebinding hecate-dev-oidc \
    --clusterrole=cluster-admin --user=olaf@hecate.test \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  cat <<EOF

Dex is running.

  export HECATE_OIDC_ISSUER=$issuer
  export HECATE_OIDC_CLIENT_ID=hecate
  export HECATE_OIDC_CLIENT_SECRET=hecate-dev-secret
  export HECATE_OIDC_REDIRECT_URL=http://127.0.0.1:18099/auth/callback
  export SSL_CERT_FILE=$OUT/ca.crt   # so hecate-api trusts the self-signed CA

  go run ./cmd/hecate-api -addr 127.0.0.1:18099 -oidc-insecure-cookie

Then open http://127.0.0.1:18099/ and sign in as olaf@hecate.test / hecate-dev.

Your browser will warn about the self-signed certificate the first time it is
sent to $host — accept it once and the flow completes.
EOF
}

# check walks the whole browser flow with curl and asserts the session works.
#
# This is the difference between "SSO is implemented" and "SSO works". Both
# failures that made it not work were invisible from the code: a bcrypt hash
# that was not the hash of the password it claimed, and a restart annotation
# over the wrong file, so a corrected config was never loaded. Nothing but
# driving the real flow would have found either.
check() {
  local api="${HECATE_API:-http://127.0.0.1:18099}"
  local jar; jar=$(mktemp)
  local host issuer state final
  host="dex.$(host_ip).nip.io"
  issuer="https://$host:5556/dex"
  # shellcheck disable=SC2064
  trap "rm -f $jar" RETURN

  curl -sf -o /dev/null "$api/healthz" \
    || die "$api is not answering — start hecate-api with the environment above"

  state=$(curl -s -L -c "$jar" -b "$jar" --cacert "$OUT/ca.crt" \
    -o /dev/null -w '%{url_effective}' "$api/auth/login" |
    grep -o 'state=[a-z0-9]*' | cut -d= -f2)
  [ -n "$state" ] || die "the login did not reach $issuer"

  final=$(curl -s -L -c "$jar" -b "$jar" --cacert "$OUT/ca.crt" \
    -d "login=olaf@hecate.test" -d "password=hecate-dev" \
    -o /dev/null -w '%{url_effective}' \
    "$issuer/auth/local/login?back=&state=$state")

  case "$final" in
    "$api"/*) ;;
    *) die "the flow ended at $final rather than back at $api — Dex refused the login" ;;
  esac
  grep -q hecate_session "$jar" || die "no session cookie was set"

  local code
  code=$(curl -s -b "$jar" -o /dev/null -w '%{http_code}' \
    "$api/api/v1alpha1/namespaces/default/gates")
  [ "$code" = "200" ] || die "the session was set but the API answered $code — \
the cluster is most likely not trusting $issuer; check \
'docker logs k3d-hecate-dev-server-0 | grep oidc'"

  log "sign-in works: Dex issued a token, the cluster accepted it, the API answered 200"
}

case "${1:-install}" in
  args) args ;;
  install) install ;;
  check) check ;;
  *) die "usage: $0 [args|install|check]" ;;
esac
