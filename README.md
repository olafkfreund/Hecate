<div align="center">

# ⛿ Hecate

**The promotion and release-orchestration layer for FluxCD.**

*Kleidouchos — the key-holder. She stands at the boundary and decides what may pass.*

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-pre--alpha-fabd2f.svg)](#status)

[Website](https://olafkfreund.github.io/Hecate/) ·
[Product plan](docs/PRODUCT-PLAN.md) ·
[Development plan](docs/DEVELOPMENT-PLAN.md) ·
[Spike results](docs/SPIKE-RESULTS.md)

</div>

---

## What

Flux makes a cluster match what is in git. It has no opinion about **what should be in git
next** — so cross-environment promotion is left to hand-rolled CI.

Argo CD users solve this with [Kargo](https://github.com/akuity/kargo). Flux users have
had nothing. The [Flux ecosystem](https://fluxcd.io/ecosystem/) has no promotion category
at all.

Hecate fills that slot:

|  | within one environment | across environments |
|---|---|---|
| **Argo CD** | Argo Rollouts | Kargo |
| **Flux** | Flagger | **Hecate** |

## How

Hecate writes rendered state into git or OCI, Flux syncs it, and Hecate reads
`Kustomization` / `HelmRelease` status back to decide whether the promotion worked.
**It never talks to Flux directly.** Git is the rendezvous — which is what keeps Flux
authoritative and Hecate removable.

```yaml
- uses: flux-wait
  config:
    resources:
      - { kind: Kustomization, name: podinfo, namespace: flux-system }
    expectedRevision: ${{ outputs.commit.commit }}
    failAfter: 15m
```

Three things distinguish it from "Kargo, but Flux":

- **Compliance-gated promotion.** A promotion *is* a change gate, and
  [Fides](https://github.com/olafkfreund/fides) already models one. Freight → trail,
  image digest → artifact, verification → attestation, approval → segregation of duties.
  "Can this ship to prod?" gets answered by evidence rather than merge rights.
- **OpenTelemetry-native.** Promotion = trace, step = span, trace context propagated
  through git commit trailers. DORA metrics fall out of the trace data.
- **A UI and CLI worth using.** Next.js 16 + React 19 + Tailwind v4, matching the Fides
  portal. The CLI is the product — every UI action has a CLI equivalent and gates return
  documented exit codes.

## Built on Kargo

Kargo is Apache-2.0 and its Argo CD coupling is narrow: two promotion steps, one health
checker, and Argo Rollouts verification — 37 of ~800 non-test Go files. So Hecate supplies
a Flux adapter and inherits the pipeline model, 34 engine-neutral promotion steps, the API
server, RBAC, SSO, sharding, GC, every git provider and every cloud registry credential
helper.

We reuse the rest of the ecosystem too: **Flux Operator** `ResourceSetInputProvider` for
artifact discovery, **Flagger** for in-environment verification.

## Repository layout

```
pkg/flux/      Flux status evaluation. No Kargo dependency — the escape hatch.
pkg/kargo/     Adapters: health.Checker + promotion.StepRunner.
docs/          Product plan, development plan, spike results.
```

`pkg/flux` is deliberately Kargo-free. All the judgement lives there, so if the
Kargo-distribution approach ever fails, that package moves into a standalone controller
unchanged.

## Status

**Pre-alpha.** The spike is complete and green — the Flux health checker and `flux-wait`
step build against Kargo v1.11.1 and pass 10 tests with no cluster required. There is no
installable release yet.

```console
$ go test ./...
ok  github.com/olafkfreund/hecate/pkg/flux    # 4 tests, pure
ok  github.com/olafkfreund/hecate/pkg/kargo   # 6 tests, fake client
```

See [`docs/SPIKE-RESULTS.md`](docs/SPIKE-RESULTS.md) for what worked, what broke, and the
two upstream PRs we owe Kargo.

## Development

```bash
go test ./...     # no cluster needed
go vet ./...
```

Kargo is consumed via `replace` directives pinning its `api` and `pkg/x/client/generated`
submodules to pseudo-versions — see the spike results for why this is currently necessary.

## Contributing

Early enough that design input matters more than code. If you run Flux across more than
one environment, [open an issue](https://github.com/olafkfreund/Hecate/issues).

## License

[Apache 2.0](LICENSE). Not affiliated with the FluxCD project, the CNCF, or Akuity.
