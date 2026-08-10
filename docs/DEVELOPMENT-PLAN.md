# Hecate — Development Plan

Companion to [PRODUCT-PLAN.md](PRODUCT-PLAN.md). This is the *how*.

---

## 1. Architecture

```
                    ┌──────────────────────────────────────────┐
   artifacts        │            HECATE control plane          │
  (images, charts,  │                                          │
   git commits)     │   Warehouse ──► Freight ──► Stage        │
        │           │                              │           │
        └──────────►│                       Promotion (steps)  │
                    │                              │           │
                    │   ┌──────────────────────────┴────────┐  │
                    │   │ git-clone · kustomize-set-image ·  │  │
                    │   │ helm-update-chart · git-commit ·   │  │
                    │   │ git-push · flux-wait ◄── ours      │  │
                    │   └──────────────────────────┬────────┘  │
                    └──────────────────────────────┼───────────┘
                          │ evidence               │ writes
                          ▼                        ▼
                    ┌──────────┐            ┌─────────────┐
                    │  FIDES   │            │  git / OCI  │  ◄── the rendezvous
                    │ change   │            └──────┬──────┘
                    │  gate    │                   │ syncs
                    └──────────┘            ┌──────▼──────┐
                          ▲                 │    FLUX     │
                          │ health/status   │ Kustomization│
                          └─────────────────┤ HelmRelease │
                                            └─────────────┘
```

**Hecate never talks to Flux.** It writes to git/OCI and reads Flux CR status. That
one rule is what keeps Flux authoritative and Hecate removable.

### Component inventory

| Component | Source | Ours? |
|---|---|---|
| Pipeline model (Stage/Freight/Promotion/Warehouse/Target) | Kargo | reused |
| 34 engine-neutral promotion steps | Kargo | reused |
| API server, RBAC, SSO/OIDC, sharding, GC, webhook receivers | Kargo | reused |
| Git providers (GitHub/GitLab/Bitbucket/Gitea/AzDO) | Kargo | reused |
| Registry credentials (ECR/GAR/ACR/GitHub App) | Kargo | reused |
| **Flux health checker + `flux-wait` / `flux-reconcile`** | — | **built** ✅ spike done |
| **Flagger verification** | — | **built** |
| **Fides change gate + attestations** | — | **built** |
| **OTel tracing / DORA** | — | **built** |
| **UI** | — | **built** (Fides portal stack) |
| **CLI** | Kargo CLI + our commands | extended |
| Artifact discovery | Flux Operator `ResourceSetInputProvider` | reused (v0.3) |

## 2. Repository layout

```
pkg/flux/          Flux status evaluation. NO Kargo dependency. The escape hatch.
pkg/kargo/         Adapters: health.Checker + promotion.StepRunner.
pkg/fides/         Fides client: change gate, attestation, approval.
pkg/telemetry/     OTel tracer/meter setup, span conventions, git trailer propagation.
cmd/hecate-controller/   Composed control plane (vendors Kargo's cmd/controlplane).
cmd/hecate/        CLI.
ui/                Next.js 16 portal.
charts/hecate/     Helm chart.
docs/              Product plan, dev plan, spike results, user docs.
examples/          Runnable Stage/Warehouse YAML.
```

**The `pkg/flux` boundary is load-bearing.** It is pure, it has no Kargo import, and it
holds all the judgement. If Path A ever collapses, that package moves into a standalone
controller unchanged. Keep it that way — no Kargo types may leak into it.

## 3. Technology decisions

| Decision | Choice | Why |
|---|---|---|
| Language (control plane, CLI) | Go 1.26 | Matches Kargo, Flux, and Fides |
| Flux API access | `unstructured` | No dependency on 4 Flux controller modules; survives version bumps |
| Kargo consumption | Go module + `replace` pseudo-versions | Proven in spike; upstream tags requested |
| UI | Next.js 16 · React 19 · Tailwind v4 · lucide-react · recharts · next-themes | Identical to Fides portal — one platform feel |
| CLI framework | cobra | Kargo and Fides both use it |
| Observability | OpenTelemetry (traces + metrics), OTLP export | Requirement; also gives DORA for free |
| Packaging | Helm chart + OCI images (GHCR) | Flux users install charts |
| Docs site | GitHub Pages | Same as Fides |

## 4. Milestones

Each milestone maps to a GitHub Epic. Child issues hang off each Epic.

### M0 — Spike ✅ **complete**
Prove the Kargo plugin points work from an external module.
**Exit:** `pkg/flux` + `pkg/kargo`, 10 tests green, three blockers documented.

### M1 — First working binary
Vendor `cmd/controlplane`, register the Flux checker and steps, ship a Helm chart, run a
real promotion against a kind cluster with Flux installed.
**Exit:** `helm install hecate` → promote podinfo dev→staging on a real cluster, in CI.

### M2 — Flux depth
`flux-reconcile` step (annotate to force sync), Flagger `Canary` verification, remote-cluster
support (gated on upstream PR #2), `ResourceSetInputProvider` as a Warehouse source.
**Exit:** a Stage can promote, force reconcile, wait, and verify via Flagger.

### M3 — Fides integration
Change gate before promotion, attestation after, approvals mapped to Fides SoD, image
digests from Freight reported as Fides artifacts.
**Exit:** a promotion to prod is blocked by a HOLD verdict and released by `fides approve`.

### M4 — OpenTelemetry
Promotion = trace, step = span. Trace context in git commit trailers. DORA metrics.
Exemplar Grafana dashboard.
**Exit:** one trace spans CI → promotion → Flux reconciliation.

### M5 — CLI
Full surface, three output formats, documented exit codes, shell completion, `hecate
promote --watch`.
**Exit:** every UI action has a CLI equivalent; gates usable in CI by exit code.

### M6 — UI
Pipeline graph, Freight timeline, Stage health, one-click promote, approval queue,
Fides evidence panel.
**Exit:** a promotion can be driven end-to-end from the browser.

### M7 — Provider matrix
Verify every major git provider and cloud registry in CI.
**Exit:** the support matrix in the README is generated from passing CI jobs, not claims.

### M8 — v1.0
SSO, RBAC docs, upgrade path, security review, ecosystem listing.

## 5. Upstream strategy

Two PRs to `akuity/kargo`, opened early because lead time is outside our control:

1. **Publish `api/vX.Y.Z` tags** so `github.com/akuity/kargo` is consumable without
   `replace` directives. Small, benefits all consumers.
2. **Generic delivery-engine client in `StepRunnerCapabilities`** (or rename
   `ArgoCDClient`). Unblocks remote-cluster Flux.

A third, offered rather than requested: **contribute the Flux checker upstream.** If
Akuity takes it, Kargo becomes Flux-capable and Hecate's differentiator moves entirely to
Fides + OTel + UI — which is where we want it anyway.

## 6. Testing strategy

| Layer | Approach | Cluster? |
|---|---|---|
| `pkg/flux` | Table-driven unit tests over unstructured fixtures | no |
| `pkg/kargo` | controller-runtime fake client | no |
| `pkg/fides` | httptest against recorded Fides responses | no |
| Controller | envtest | no |
| End-to-end | kind + Flux + Flagger + a real git provider, in CI | yes |
| Provider matrix | Nightly, one job per git provider / registry | yes |

Rule: **anything that can be tested without a cluster must be.** The spike's 10 tests run
in 0.1s; that is the bar.

## 7. Release strategy

- SemVer. `v0.x` until the CRD contract is stable.
- Images to GHCR, chart to an OCI registry (Flux users consume charts via `OCIRepository`).
- Every release signed with cosign, SBOM attached — and, fittingly, **its own provenance
  recorded in Fides**. Hecate should be the first thing gated by Hecate.
- Release notes generated from Epic/issue links.

## 8. Definition of done (per issue)

- [ ] Tests, and they run without a cluster where possible
- [ ] `go vet` and `gofmt` clean
- [ ] User-facing docs updated
- [ ] An `examples/` entry if it adds a step, checker, or CRD field
- [ ] OTel spans if it touches the promotion path
