<div align="center">

# ⛿ Hecate

**The promotion and release-orchestration layer for FluxCD.**

*Kleidouchos — the key-holder. She stands at the boundary and decides what may pass.*

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-pre--alpha-fabd2f.svg)](#status)

[Website](https://olafkfreund.github.io/Hecate/) ·
[Architecture](docs/ARCHITECTURE.md) ·
[Product plan](docs/PRODUCT-PLAN.md) ·
[Development plan](docs/DEVELOPMENT-PLAN.md) ·
[Decisions](docs/DECISIONS.md)

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
| **Beacon** | Watches registries, charts and repos. Emits a Bundle when something new appears. |
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
      - uses: set-image
      - uses: git-commit
        as: commit
      - uses: git-push
      - uses: flux-wait
        with:
          resources:
            - { kind: Kustomization, name: podinfo, namespace: flux-system }
          expectedRevision: ${{ steps.commit.sha }}
```

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
- **OpenTelemetry-native.** Passage = trace, step = span, trace context propagated
  through git commit trailers. `traceID` is a first-class API field. DORA metrics fall
  out of the trace data.
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

## Repository layout

```
api/v1alpha1/         Beacon, Bundle, Gate, Passage. The whole API.
pkg/flux/             Flux status evaluation. Pure; no client, no I/O.
pkg/health/           Checker interface, registry, and the Flux checker.
pkg/passage/          Step interface, registry, and the execution engine.
pkg/passage/steps/    Built-in steps.
charts/hecate/crds/   Generated CRDs.
```

## Compatibility

| | Supported |
|---|---|
| **Flux** | The three minor versions Flux itself supports (currently v2.7–v2.9) |
| **Kubernetes** | N-2, matching Flux |
| **Hecate** | The latest two minors |

Hecate reads Flux resources and never writes them. The exact API versions and status
fields it depends on are listed in
[the Flux compatibility surface](docs/ARCHITECTURE.md#flux-compatibility-surface) —
that list is the blast radius of any Flux change.

Cross-namespace references are **refused by default**, matching Flux's own
`--no-cross-namespace-refs=true` posture. See [D11](docs/DECISIONS.md).

We track Flux with CI rather than good intentions: the e2e matrix runs against every
supported Flux minor, Renovate opens the bump PR on each Flux release, and `pkg/flux`
tests read real captured status output per version. Details in the
[development plan](docs/DEVELOPMENT-PLAN.md#6-following-flux-upstream).

## Status

**Pre-alpha.** The foundations are built and tested; there is no installable release
yet. See the [development plan](docs/DEVELOPMENT-PLAN.md) for what lands when.

```console
$ go test ./...
ok  github.com/olafkfreund/hecate/api/v1alpha1
ok  github.com/olafkfreund/hecate/pkg/flux
ok  github.com/olafkfreund/hecate/pkg/health
ok  github.com/olafkfreund/hecate/pkg/passage
ok  github.com/olafkfreund/hecate/pkg/passage/steps
```

33 tests, no cluster required, ~0.2s. That is the bar: anything testable without a
cluster must be.

## Development

```bash
go test ./...
go vet ./...

# regenerate deepcopy functions and CRDs after changing api/
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 \
  object:headerFile=/dev/null paths=./api/... \
  crd output:crd:dir=charts/hecate/crds
```

## Contributing

Early enough that design input matters more than code. If you run Flux across more
than one environment, [open an issue](https://github.com/olafkfreund/Hecate/issues).

Non-obvious decisions are recorded in [docs/DECISIONS.md](docs/DECISIONS.md) — read it
before proposing a structural change; the answer may already be there.

## License

[Apache 2.0](LICENSE). Not affiliated with the FluxCD project or the CNCF.
