#!/usr/bin/env bash
# Record a Hecate release in Fides.
#
# Hecate's pitch is that a promotion should be gated on recorded evidence. A
# project making that argument while shipping its own releases with no record
# at all is asking for something it will not do (#57).
#
# WHAT THIS DELIBERATELY DOES NOT DO. It does not gate. Fides refusing a Hecate
# release would mean the evidence system can stop the tool that reads it from
# shipping a fix — including a fix to the integration between them. Recording is
# the honest half to dogfood; gating our own release on our own dependency is a
# loop, not a demonstration.
#
# Prints one line to stdout describing the outcome: RECORDED, SKIPPED or FAILED.
# The caller puts that in the release notes, because "no evidence was recorded"
# and "the evidence is fine" are different answers and a release that cannot
# tell them apart is the failure mode this whole feature exists to prevent
# (D50).
#
# Exit codes: 0 recorded or skipped; 1 configured but the recording failed.
set -euo pipefail

: "${VERSION:?VERSION is required, e.g. 0.3.0}"
: "${IMAGE:=ghcr.io/olafkfreund/hecate-controller}"
FLOW="${FIDES_FLOW:-hecate}"

# Unconfigured is a normal state for a fork, a dry run, or anyone building this
# who has no Fides. It is reported rather than hidden: the whole lesson of the
# five weeks ARC spent posting evidence to a placeholder token is that a silent
# skip and a silent failure look identical from the outside.
if [ -z "${FIDES_SERVER_URL:-}" ] || [ -z "${FIDES_API_TOKEN:-}" ]; then
  echo "SKIPPED: no Fides configured (FIDES_SERVER_URL/FIDES_API_TOKEN unset)"
  exit 0
fi

BASE="${FIDES_SERVER_URL%/}"
AUTH=(-H "Authorization: Bearer ${FIDES_API_TOKEN}")
JSON=(-H "Content-Type: application/json" -H "Accept: application/json")

# `-f` is absent on purpose. It exits 22 and prints nothing on an HTTP error,
# which is how the equivalent script elsewhere managed to fail silently for
# weeks. Status and body are captured so a failure can say what happened.
api() {
  local method="$1" path="$2" body="${3:-}" code
  if [ -n "$body" ]; then
    code=$(curl -sS -o /tmp/fides.out -w '%{http_code}' --max-time 30 \
      "${AUTH[@]}" "${JSON[@]}" -X "$method" "$BASE$path" -d "$body")
  else
    code=$(curl -sS -o /tmp/fides.out -w '%{http_code}' --max-time 30 \
      "${AUTH[@]}" "${JSON[@]}" -X "$method" "$BASE$path")
  fi
  case "$code" in
    2*) cat /tmp/fides.out ;;
    *) echo "FAILED: $method $path returned $code: $(head -c 200 /tmp/fides.out)" >&2; return 1 ;;
  esac
}

fail() { echo "FAILED: $*"; exit 1; }

# --- the flow ---------------------------------------------------------------

flow_id=$(api GET "/api/v1/flows" | jq -r --arg n "$FLOW" '.[]? | select(.name==$n) | .id' | head -1) \
  || fail "could not list flows"

if [ -z "$flow_id" ]; then
  flow_id=$(api POST "/api/v1/flows" "$(jq -n --arg n "$FLOW" \
    '{name:$n, description:"Hecate'"'"'s own releases"}')" | jq -r '.id') \
    || fail "could not create the flow $FLOW"
fi
[ -n "$flow_id" ] || fail "no flow id for $FLOW"

# --- the trail --------------------------------------------------------------

# Named for the version rather than the commit, because a release is the unit
# someone asks about: "what shipped in 0.3.0" is the question, and a commit sha
# needs a lookup before it answers it.
trail="v${VERSION}"
trail_id=$(api GET "/api/v1/flows/${flow_id}/trails" | jq -r --arg n "$trail" \
  '.[]? | select(.name==$n) | .id' | head -1) || fail "could not list trails"

if [ -z "$trail_id" ]; then
  trail_id=$(api POST "/api/v1/trails" "$(jq -n --arg f "$flow_id" --arg n "$trail" \
    '{flow_id:$f, name:$n}')" | jq -r '.id') || fail "could not create trail $trail"
fi
[ -n "$trail_id" ] || fail "no trail id for $trail"

# --- the artifact -----------------------------------------------------------

# By digest, not by tag. A tag is a name someone can move; the digest is what
# was actually published, and it is the only identifier an audit can rely on.
if [ -n "${IMAGE_DIGEST:-}" ]; then
  hex="${IMAGE_DIGEST#sha256:}"
  case "$hex" in
    [0-9a-f]*) ;;
    *) fail "IMAGE_DIGEST is not a sha256 digest: ${IMAGE_DIGEST}" ;;
  esac
  api POST "/api/v1/artifacts" "$(jq -n --arg t "$trail_id" --arg s "$hex" --arg n "hecate-controller" \
    '{trail_id:$t, sha256:$s, name:$n, type:"docker"}')" >/dev/null \
    || fail "could not record the image artifact"
fi

echo "RECORDED: ${trail} in Fides flow ${FLOW} (${BASE})"
