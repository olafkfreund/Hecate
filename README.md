<div align="center">

# ⛿ Hecate

**The promotion and release-orchestration layer for FluxCD.**

*Kleidouchos — the key-holder. She stands at the boundary and decides what may pass.*

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-pre--alpha-fabd2f.svg)](#status)

[Website](https://olafkfreund.github.io/Hecate/) ·
[Architecture](docs/ARCHITECTURE.md) ·
[Steps](docs/STEPS.md) ·
[Product plan](docs/PRODUCT-PLAN.md) ·
[Development plan](docs/DEVELOPMENT-PLAN.md) ·
[Permissions](docs/RBAC.md) ·
[Signing in](docs/SSO.md) ·
[Observability](docs/OBSERVABILITY.md) ·
[Decisions](docs/DECISIONS.md) ·
[Onboarding](docs/ONBOARDING.md) ·
[Contributing](CONTRIBUTING.md)

</div>

---

## What

Flux makes a cluster match what is in git. It has no opinion about **what should be in
git next** — so cross-environment promotion is left to hand-rolled CI.

The [Flux ecosystem](https://fluxcd.io/ecosystem/) has no promotion category at all.
Hecate fills that slot:

|  | within one environment | across environments |
|---|---|---|
| **Argo CD** | Argo Rollouts | (well served) |
| **Flux** | Flagger | **Hecate** |

## The model

Four resources. That is the whole API.

| | |
|---|---|
| **Beacon** | Watches registries, charts, repos — or a Flux Operator `ResourceSetInputProvider`. Emits a Bundle when something new appears. |
| **Bundle** | An immutable, content-addressed set of artifact versions. The unit that moves. |
| **Gate** | An environment, and the threshold a Bundle must cross to enter it. |
| **Passage** | One attempt to move one Bundle through one Gate. |

```yaml
apiVersion: hecate.dev/v1alpha1
kind: Gate
metadata:
  name: production
spec:
  admits:
    - from: { beacon: podinfo }
      after: [staging]          # only what already cleared staging
      requireApproval: true
  passage:
    steps:
      - uses: git-clone
        with: { repo: https://github.com/acme/fleet.git }
      - uses: set-image
        with:
          path: repo/apps/production/kustomization.yaml
          image: ghcr.io/acme/podinfo   # the version comes from the Bundle
      - uses: git-commit
        as: commit
        with: { message: "promote podinfo to production" }
      - uses: git-push
      - uses: flux-reconcile          # sync now, not at the next interval
        with:
          resources:
            - { kind: GitRepository, name: fleet, namespace: flux-system }
      - uses: flux-wait
        with:
          resources:
            - { kind: Kustomization, name: podinfo, namespace: flux-system }
          expectedRevision: ${{ steps.commit.sha }}
```

### Reusing Flux Operator's discovery

Flux Operator's `ResourceSetInputProvider` already discovers from GitHub, GitLab,
Azure DevOps, AWS CodeCommit, Gitea, OCI, ACR, ECR and GAR. A Beacon can read one
instead of doing it again:

```yaml
watch:
  - provider: { name: podinfo-releases }
```

It names the provider and nothing else. The filter, the semver range and the
limit live there, and the provider **already sorts what it exports** — so Hecate
takes the newest and does not apply a second opinion about what "newest" means.

### Discovery without waiting for the interval

A Beacon polls on its own schedule. To make a push land in seconds instead,
have CI tell it to look now:

```yaml
# .github/workflows/release.yml
- name: Tell Hecate to look
  run: |
    curl -fsS -X POST \
      -H "Authorization: Bearer $(cat $ACTIONS_ID_TOKEN_FILE)" \
      https://hecate.acme.example/api/v1alpha1/namespaces/acme/beacons/podinfo/poll
```

No shared secret and no HMAC to verify: the call is authenticated the way every
other call to the API is, by asking Kubernetes to review the bearer token. A
cluster configured to trust your CI provider's OIDC issuer accepts its workload
token here with nothing added — the same posture Flux v2.9 moved to with
OIDC-secured Receivers. The identity needs `update` on `beacons` and nothing
else, so a CI job that may poke a Beacon still cannot read your Gates.

### Asking what is going on

```console
$ hecate status
GATE        STATE     CURRENT      HEALTH   SUMMARY
dev         Idle      podinfo-6b2  Healthy  nothing to do — everything admitted is already in this Gate
staging     Crossing  podinfo-4a1  Healthy  crossing podinfo-6b2 — waiting on flux-wait: Kustomization not yet at the revision
production  Blocked   podinfo-4a1  Healthy  1 Bundle cannot cross: awaiting approval

$ hecate explain production
production is Blocked
1 Bundle cannot cross: awaiting approval

  [AwaitingApproval] awaiting approval
      → hecate approve podinfo-6b2
  waiting:  podinfo-6b2 — awaiting approval
```

Every read command takes `-o table|json|yaml`. The structured formats serialise
the same value the table is built from, so they cannot drift apart, and the exit
code is the same either way — `hecate verify -o json` still exits 3 on a broken
chain, because a format that always exited 0 would break the scripting it exists
for.

```console
$ hecate status -o json | jq -r '.[] | select(.state == "Blocked") | .gate'
production
```

### As a Flux CLI plugin

The same binary, reached through the CLI you already have. Flux discovers
plugins in `~/.fluxcd/plugins`, so installing it is a copy:

```console
$ mkdir -p ~/.fluxcd/plugins
$ cp hecate ~/.fluxcd/plugins/flux-hecate
$ flux plugin list
NAME    VERSION  PATH
hecate  manual   /home/you/.fluxcd/plugins/flux-hecate

$ flux hecate status
```

It knows how it was reached: `flux hecate help` offers `flux hecate promote`,
not `hecate promote` — a binary that is not on your PATH when it lives in the
plugin directory.

Completion knows your cluster, not just the verbs — `hecate explain <TAB>`
offers the Gates that exist, `hecate abort <TAB>` only the crossings still
running:

```console
$ source <(hecate completion bash)     # or zsh, or fish
$ hecate man > /usr/share/man/man1/hecate.1
```

### Promoting, and waiting for it

```console
$ hecate promote production --bundle podinfo-6b2 --watch
crossing podinfo-6b2 through production as production-vck6g
  git-clone        Succeeded  checked out fleet at fa90646e
  set-image        Succeeded  pinned ghcr.io/acme/podinfo to 6.14.1
  git-commit       Succeeded  committed 9f8c1a2b
  git-push         Succeeded  pushed 9f8c1a2b to main
  flux-wait        Succeeded  Flux applied the promoted revision to 1 resource(s)

podinfo-6b2 crossed production
```

`--watch` exists for its **exit code**. Without it a CI job learns only that a
Passage was opened, which is not the question it asked.

| exit | meaning |
|---|---|
| 0 | it worked |
| 1 | you used the command wrong |
| 2 | Hecate could not tell you — the cluster, a timeout, a stopped watch |
| 3 | an evidence chain is broken (`verify`) |
| 4 | nothing has been recorded (`verify`) |
| 5 | the rules said no — not cleared upstream, a closed window, no approval |
| 6 | the crossing ran and did not land |

**2 and 6 are deliberately different.** "The promotion failed" and "I lost sight
of the cluster" call for different responses, and only the first is a deployment
failure. **5 is different again**: it is an answer, not a malfunction.

"Why is nothing crossing?" is otherwise answered by reading four resources and
knowing which fields matter. The answer is computed in one place —
[`pkg/ops`](docs/DECISIONS.md) — so the CLI, the API and the UI cannot come to
different conclusions about the same cluster.

### Asking through an LLM

```console
$ hecate-mcp -namespace acme      # stdio, read-only
```

An MCP server over the same operations layer, so an agent and the CLI cannot
come to different conclusions. `why_stuck` returns typed blockers as
`structuredContent`, which is what makes it something a model reasons over
rather than parses out of prose.

It speaks both MCP eras — the stateless `2026-07-28` revision and the older
handshake-based ones — because a modern-only server cannot be used by the
clients most people are running today ([D33](docs/DECISIONS.md)).

It reads by default. `--allow-writes --actor <you>` adds `promote` and `abort`,
judged by the same rules as every other path. **It can never approve**, and
there is no flag for it: approval is a segregation-of-duties control, and an
agent that could satisfy it would leave the control in every audit trail having
stopped meaning anything ([D34](docs/DECISIONS.md)).

### Diagnosis, optionally with a model

```console
$ export HECATE_LLM_URL=http://localhost:11434/v1 HECATE_LLM_MODEL=llama3.2
$ hecate explain production --ai
production is Blocked
1 Bundle cannot cross: awaiting approval

  [AwaitingApproval] awaiting approval
      → hecate approve podinfo-6b2

llama3.2 says:
  The production gate is blocked: podinfo-6b2 is waiting for approval.
  Run `hecate approve podinfo-6b2` to let it through.
```

Any OpenAI-compatible endpoint — Ollama, llama.cpp, vLLM, or a hosted vendor —
because they all speak the same API. **The model phrases the diagnosis; it never
makes it.** It is handed the analysis Hecate already did, everything works
identically without one, and nothing it says affects a promotion
([D35](docs/DECISIONS.md)).

### Compliance as a gate, not a report

A Gate can refuse a crossing on evidence. All four of
[Fides](https://github.com/olafkfreund/evidance-vault)'s gates are available as
one step — is the artifact compliant, is it approved for this environment, does
the environment's policy accept the build, and what does the change gate say:

```yaml
    steps:
      - uses: evidence-gate
        with:
          gates: [assert, allowlist, policy, change]
          maxRisk: 40
```

A `hold` waits for the human to sign off rather than failing the promotion; the
other three end it. The evidence judged is the trail CI recorded when it built
the image, so the gates see the SBOM and scans that actually exist.

A Gate says which Fides environment it is:

```yaml
spec:
  evidence:
    fidesEnvironment: 7f3a1c2e-8b41-4d6a-9e2f-0000000009b04   # validated on apply
    credentialsRef: { name: fides }
```

The trail is tamper-evident, and the check is yours to run rather than ours to
claim:

```console
$ hecate verify podinfo-abc123
✓ staging (trail aaaa1111)
  chain valid — 9 attestations, anchored 2026-08-09
✗ production (trail 91b2ffff)
  chain BROKEN at entry 6: content_hash does not match the recorded entry
```

Exit 3 when a chain is broken, 2 when it could not be checked, 4 when nothing
has been recorded — never 0 for any of them.

**There is no pipeline object** — the graph is implied by what each Gate admits, so it
cannot drift out of sync with reality.

**Hecate never talks to Flux.** A Passage writes to git; Flux syncs; Hecate reads
`Kustomization` / `HelmRelease` status back. Git is the rendezvous, which keeps Flux
authoritative and Hecate removable.

## What makes it different

- **Evidence-gated crossings.** A crossing *is* a change gate, and
  [Fides](https://github.com/olafkfreund/fides) already models one: Bundle → trail,
  image digest → artifact, verification → attestation, approval → segregation of
  duties. "Can this ship to prod?" gets answered by evidence rather than merge rights.
- **OpenTelemetry-native.** Passage = trace, step = span, and `traceID` is a
  first-class API field rather than an annotation bolted on later. Point
  `OTEL_EXPORTER_OTLP_ENDPOINT` at your collector and crossings show up there; every
  other knob is the standard `OTEL_*` variable, and with none of them set nothing is
  exported. Every promotion commit carries a `traceparent` trailer, so one trace spans
  the CI run, the crossing and the Flux reconciliation. The DORA four and a Grafana
  dashboard come with it — see [observability](docs/OBSERVABILITY.md), which is candid
  about which two of the four Hecate can measure exactly and which two it approximates.
- **A UI and CLI worth using.** Next.js 16 + React 19 + Tailwind v4, matching the
  Fides portal. The CLI is the product — every UI action has a CLI equivalent and
  gates return documented exit codes.

## Honest health

Most naive Flux health checks are wrong in the same ways. These are all tested:

- **Stale status is the false-green.** `Ready=True` with `observedGeneration` behind
  `metadata.generation` describes the *previous* revision.
- **`Ready=False` is not failure.** Flux retries forever; we report Progressing until
  `Stalled=True` or a tunable deadline passes.
- **Suspended is not progressing.** It will never reconcile — waiting is pointless.
- **An unregistered checker is reported, not skipped.** Silently ignoring a check you
  asked for inflates Gate health.
- **A never-reconciled resource is judged by its conditions.** Flux writes
  `observedGeneration: -1` there while the conditions are already current, so reading
  the top-level field reports a source that will never work as one still starting up.

The status these are tested against is [captured from a real
Flux](pkg/flux/testdata/), not written by hand — the last one on that list was a live
bug that only real output revealed.

## Repository layout

```
api/v1alpha1/         Beacon, Bundle, Gate, Passage. The whole API.
pkg/beacon/           Artifact discovery: tag selection, image resolution, controller.
pkg/expr/             ${{ }} evaluation. Sandboxed; no I/O, no arbitrary code.
pkg/flux/             Flux status evaluation. Pure; no client, no I/O.
pkg/gate/             Eligibility, promotion windows, Gate controller.
pkg/health/           Checker interface, registry, and the Flux checker.
pkg/passage/          Step interface, registry, execution engine, Passage controller.
pkg/passage/steps/    Built-in steps.
pkg/metrics/          Delivery metrics: crossings, step durations, Gate health.
pkg/telemetry/        OpenTelemetry wiring, configured entirely by OTEL_*.
ui/                   The web portal. Next.js, built into the API binary.
charts/hecate/        Helm chart. CRDs and RBAC are generated, never hand-edited.
charts/hecate/crds/   Generated CRDs.
```

## Upgrading

CRDs first, then the chart. Helm installs a chart's `crds/` once and never
touches them again, so upgrading the chart alone runs a new controller against
the old API — and the API server prunes unknown fields silently rather than
rejecting them.

```console
$ kubectl apply --server-side -f \
    https://github.com/olafkfreund/Hecate/releases/download/v0.1.0/crds.yaml
$ helm upgrade hecate oci://ghcr.io/olafkfreund/charts/hecate --version 0.1.0 \
    --namespace hecate-system
```

Skipping the first step is a failed rollout rather than a silent behaviour
change: the controller refuses to start against CRDs older than itself and
names the fields that are missing, while the previous version keeps running.

**A crossing already under way survives.** Finished steps are not re-run, the
running one is re-entered where it was, and the crossing finishes under the
rules it started with — an upgrade cannot retroactively change what an
in-flight Passage is doing. [The details, and how they were
measured](docs/ARCHITECTURE.md#upgrading-the-control-plane).

`v1alpha1` promises no field stability; that is what alpha means. A stable
field list is a v1 deliverable.

## Compatibility

| | Supported | Proved by |
|---|---|---|
| **Flux** | The three minors Flux itself supports — currently v2.7, v2.8, v2.9 | every e2e run, one leg each |
| **Kubernetes** | N-2 — currently v1.34, v1.35, v1.36 | the same three legs, one version each |
| **Hecate** | The latest two minors | nothing yet; there is no release |

Each e2e leg pairs one Flux with one Kubernetes, oldest with oldest, so three jobs cover
both ranges. **The cross product is not tested** — new Flux on old Kubernetes is not a
combination CI exercises — and the pairing puts together the versions most likely to
break rather than pretending to more coverage than nine jobs would buy.

The API is `v1alpha1` and can still change incompatibly. Deprecations follow Flux's own
rules — three months' notice once a replacement exists, and a removal that fails the
rollout rather than silently pruning fields. Read the
[full policy](docs/DEVELOPMENT-PLAN.md#api-lifecycle) before depending on it.

Hecate reads Flux resources and never writes them. The exact API versions and status
fields it depends on are listed in
[the Flux compatibility surface](docs/ARCHITECTURE.md#flux-compatibility-surface) —
that list is the blast radius of any Flux change.

Cross-namespace references are **refused by default**, matching Flux's own
`--no-cross-namespace-refs=true` posture. See [D11](docs/DECISIONS.md).

We track Flux with CI rather than good intentions: the e2e matrix runs against every
supported Flux minor, a weekly job opens an issue when a new Flux minor appears, the
controller warns at startup if the cluster serves an API version it does not expect, and
`pkg/flux` tests read status captured from a real Flux, per version. Details in the
[development plan](docs/DEVELOPMENT-PLAN.md#6-following-flux-upstream).

## Status

**Pre-alpha.** The foundations are built and tested; there is no installable release
yet. See the [development plan](docs/DEVELOPMENT-PLAN.md) for what lands when.

```console
$ go test ./...
ok  github.com/olafkfreund/hecate/api/v1alpha1
ok  github.com/olafkfreund/hecate/pkg/beacon
ok  github.com/olafkfreund/hecate/pkg/flux
ok  github.com/olafkfreund/hecate/pkg/gate
ok  github.com/olafkfreund/hecate/pkg/health
ok  github.com/olafkfreund/hecate/pkg/passage
ok  github.com/olafkfreund/hecate/pkg/passage/steps
ok  github.com/olafkfreund/hecate/pkg/telemetry
```

353 tests, no cluster required, ~1s. That is the bar: anything testable without a
cluster must be. Image resolution is tested against a real in-memory registry rather
than a mock.

## Development

Everything is pinned in `flake.nix` — Go, kubectl, Helm, k3d, the Flux CLI,
controller-gen — at the versions CI uses.

```bash
nix develop          # or: direnv allow

make test            # 353 tests, ~1s, no cluster
make cluster         # k3d in Docker, with Flux installed
make install         # build, push and install the chart into the dev cluster
make e2e             # drive a Bundle through two Gates on a real API server
make ui              # build the web portal into the API binary
make generate        # regenerate CRDs and RBAC after touching api/
make check           # what CI enforces
```

`make cluster` pins `KUBECONFIG` to `./.dev/kubeconfig`, so a stray `kubectl
delete` during development can never reach a real cluster.

Full setup, including agenix secrets: **[docs/ONBOARDING.md](docs/ONBOARDING.md)**.

## Contributing

Early enough that design input matters more than code. If you run Flux across more
than one environment, [open an issue](https://github.com/olafkfreund/Hecate/issues).

Non-obvious decisions are recorded in [docs/DECISIONS.md](docs/DECISIONS.md) — read it
before proposing a structural change; the answer may already be there.

## License

[Apache 2.0](LICENSE). Not affiliated with the FluxCD project or the CNCF.
