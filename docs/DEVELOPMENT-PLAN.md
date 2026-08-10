# Hecate — Development Plan

Companion to [PRODUCT-PLAN.md](PRODUCT-PLAN.md) and [ARCHITECTURE.md](ARCHITECTURE.md).
This is the *how*.

---

## 1. What exists

| Component | State |
|---|---|
| `api/v1alpha1` — Beacon, Bundle, Gate, Passage + generated CRDs | ✅ done |
| `pkg/flux` — Flux status evaluation | ✅ done |
| `pkg/health` — Checker interface, registry, Flux checker | ✅ done |
| `pkg/passage` — Step interface, registry, execution engine | ✅ done |
| `pkg/passage/steps` — `flux-wait` | ✅ done |
| Controllers | M1 |
| Step library (git, render, OCI) | M2 |
| Providers (git hosts, registries) | M3 |
| Fides evidence | M4 |
| OpenTelemetry | M5 |
| CLI | M6 |
| API server | M7 |
| UI | M8 |

33 tests, no cluster required, ~0.2s.

## 2. Technology

| Decision | Choice | Why |
|---|---|---|
| Language | Go 1.26 | Matches Flux, Fides, and the ecosystem we integrate with |
| Controller framework | controller-runtime | The default; no reason to be exotic |
| Flux API access | `unstructured` | No dependency on four Flux modules; survives version bumps |
| Git | `go-git`, shelling out only where it cannot | Most of a "git step" is wiring, not algorithms |
| Helm | `helm.sh/helm/v3` SDK | Template and chart resolution without a binary |
| Kustomize | `sigs.k8s.io/kustomize/api` | Same |
| Registries | `google/go-containerregistry` | Tag listing, digest resolution, cloud keychains for free |
| Expressions | `expr-lang/expr` | Sandboxed, fast, no template injection surface |
| Observability | OpenTelemetry (traces + metrics), OTLP | Requirement; DORA falls out of it |
| UI | Next.js 16 · React 19 · Tailwind v4 · lucide-react · recharts | Identical to the Fides portal |
| CLI | cobra | Convention |
| Packaging | Helm chart + OCI images on GHCR | Flux users consume charts via `OCIRepository` |

**The registry/git/helm choices are the scope mitigation.** Most of what a promotion
engine does in that layer is call these libraries. We call them directly.

## 3. Milestones

Each maps to a GitHub Epic.

### M0 — Foundations ✅
API, status evaluation, health framework, step engine, first step.

### M1 — Control plane
Controllers for all four kinds: Beacon discovers and emits Bundles; Gate computes
eligibility and health; Passage runs the engine. Helm chart, RBAC, GHCR images.

**Exit:** `helm install hecate`, then a Bundle crosses a Gate on a kind cluster
running Flux, in CI.

### M2 — Step library
`flux-reconcile`, `git-clone`, `git-commit`, `git-push`, `git-pull-request`,
`render-kustomize`, `render-helm`, `set-image`, `edit-yaml`, `oci-push`, `http`.
Expression evaluation over Bundle artifacts and prior step outputs. Per-step JSON
Schema for validation and UI form generation.

**Exit:** a Gate can render, commit, push, open a PR, wait for merge, and wait for Flux.

### M3 — Providers
Git hosts (GitHub, GitLab, Bitbucket, Gitea, Azure DevOps, AWS CodeCommit) and
registries (ECR, GAR, ACR, GHCR, Docker Hub, Quay, Harbor), including workload
identity. Inbound webhooks so a push triggers discovery instead of waiting for the
poll interval.

**Exit:** the support matrix in the README is generated from passing CI jobs.

### M4 — Evidence
Fides client, `evidence-gate` step, attestation per Passage, approvals mapped to Fides
segregation of duties, Bundle image digests reported as Fides artifacts, verdict and
risk surfaced on the Gate.

**Exit:** a crossing into production is blocked by a HOLD verdict and released by an
approval.

### M5 — OpenTelemetry
Tracer bootstrap, Passage traces and step spans, trace context in git commit trailers,
DORA metrics, exemplar Grafana dashboard.

**Exit:** one trace spans CI → crossing → Flux reconciliation.

### M6 — CLI
Full surface, three output formats, documented exit codes, `hecate cross --watch`,
shell completion.

**Exit:** every operation is scriptable; gates usable in CI by exit code alone.

### M7 — API server
Read/write API, SSO/OIDC, RBAC. The UI's backend.

### M8 — UI
Pipeline graph, Bundle timeline, Gate health detail, one-click crossing, approval
queue, evidence panel, theme parity with Fides.

### M9 — v1.0
CRD stability, upgrade path, security review, cosign + SBOM, ecosystem listing.

## 4. Sequencing rationale

The CLI (M6) lands *before* the UI (M8) and the API server (M7) lands between them.
That order is deliberate: the CLI proves the API surface is coherent, and it is what
early adopters actually use. A UI built before the model settles is rework.

Fides (M4) and OTel (M5) come before both, because they are the differentiators and
because retrofitting tracing is far more expensive than building with it.

## 5. Testing

| Layer | Approach | Cluster? |
|---|---|---|
| `pkg/flux` | Table-driven fixtures | no |
| `pkg/health`, `pkg/passage` | Fake clients, scripted runners | no |
| `pkg/fides` | httptest against recorded responses | no |
| Controllers | envtest | no |
| End to end | kind + Flux + Flagger + a real git provider | yes |
| Providers | Nightly, one job each | yes |

**Rule: anything that can be tested without a cluster must be.** The current suite
runs in 0.2s; that is the bar. End-to-end tests catch what fixtures cannot — real Flux
status output — and are not a substitute for unit tests.

## 6. Release

- SemVer; `v0.x` until the CRD contract is stable.
- Images to GHCR, chart to an OCI registry.
- Signed with cosign, SBOM attached, and **each release's provenance recorded in
  Fides**. If we will not gate our own releases with it, we should not ask anyone else
  to.

## 7. Definition of done

- [ ] Tests, running without a cluster wherever possible
- [ ] `gofmt` and `go vet` clean
- [ ] Docs updated
- [ ] An `examples/` entry if it adds a step, checker, or API field
- [ ] A [decision record](DECISIONS.md) entry if it settles a non-obvious question
- [ ] OTel spans if it touches the Passage path
