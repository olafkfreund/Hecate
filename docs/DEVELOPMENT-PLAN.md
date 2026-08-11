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
| `pkg/beacon` — discovery, image resolution, Beacon controller | ✅ done |
| `pkg/gate` — eligibility, promotion windows, Gate controller | ✅ done |
| `pkg/passage` — execution engine and Passage controller | ✅ done |
| `cmd/hecate-controller` — the binary | ✅ done |
| Nix flake, k3d dev cluster, CI workflows | ✅ done |
| Helm chart, RBAC wiring, in-cluster e2e | M1 |
| Step library (git, render, OCI) | M2 |
| Providers (git hosts, registries) | M3 |
| Fides evidence | M4 |
| OpenTelemetry | M5 |
| CLI + `pkg/ops` | M6 |
| MCP server, LLM assist | M6.5 |
| API server | M7 |
| UI | M8 |

97 tests, no cluster required, ~1s.

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
| LLM access | One OpenAI-compatible HTTP client | Ollama, llama.cpp, vLLM and the hosted vendors all speak `/v1/chat/completions` |
| MCP | Hand-rolled JSON-RPC | Small protocol; a dependency buys nothing |
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

Ships with the safety properties that are cheap now and breaking later: cross-namespace
references refused by default ([D11](DECISIONS.md)), Bundle garbage collection and
capped status lists ([D13](DECISIONS.md)), and Kubernetes Events so Flux's
notification-controller handles alerting ([D12](DECISIONS.md)).

**Exit:** `helm install hecate`, then a Bundle crosses a Gate on a kind cluster
running Flux, in CI.

### M2 — Step library
`flux-reconcile`, `git-clone`, `git-commit`, `git-push`, `git-pull-request`,
`render-kustomize`, `render-helm`, `set-image`, `edit-yaml`, `oci-push`, `http`.
Expression evaluation over Bundle artifacts and prior step outputs. Per-step JSON
Schema for validation and UI form generation.

**Exit:** a Gate can render, commit, push, open a PR, wait for merge, and wait for Flux.

### M3 — Providers
**GitHub and GitLab first**, both including their self-hosted forms (Enterprise Server,
self-managed) from day one rather than as a retrofit. Bitbucket, Gitea and Azure DevOps
follow demand rather than a completeness urge.

The surface is smaller than it looks: clone, commit and push work against **any** host
over HTTPS or SSH with no provider code at all. Host-specific code is needed only for
pull/merge requests, commit status, and inbound webhooks — and for webhooks we follow
Flux v2.9's OIDC-secured Receivers rather than inventing shared-secret handling.

Registries (ECR, GAR, ACR, GHCR, Docker Hub, Quay, Harbor) come via
`go-containerregistry`, whose keychains cover most cloud identity cases.

**Exit:** the support matrix in the README is generated from passing CI jobs.

### M4 — Evidence
Fides client, `evidence-gate` step covering **all four** Fides gates (`assert`,
`policy check`, `allowlist check`, `change-gate` — two of which are environment-scoped
and map onto a Gate), Gate-to-Fides-environment mapping, attestation per Passage,
approvals mapped to segregation of duties, Bundle image digests reported as artifacts,
verdict and risk surfaced on the Gate, and `hecate verify` so the tamper-evidence claim
is checkable rather than asserted.

**Exit:** a crossing into production is blocked by a HOLD verdict and released by an
approval.

### M5 — OpenTelemetry
Tracer bootstrap, Passage traces and step spans, trace context in git commit trailers,
DORA metrics, exemplar Grafana dashboard.

**Exit:** one trace spans CI → crossing → Flux reconciliation.

### M6 — CLI, over a shared operations layer
`pkg/ops` first — one implementation of "list, explain, cross, approve, abort" that the
CLI, API server, MCP server and UI all call. Then the CLI on top: full surface, three
output formats, documented exit codes, `hecate cross --watch`, shell completion.

**Exit:** every operation is scriptable; gates usable in CI by exit code alone.

### M6.5 — MCP server and LLM assist
An MCP server so LLM clients can drive Hecate, and an optional LLM to help explain a
stuck Passage. Both are thin adapters over `pkg/ops`, which is why they sit here rather
than earlier: built before it, they would each grow their own copy of the rules.

One OpenAI-compatible client covers Ollama, llama.cpp, vLLM and the hosted vendors —
no provider abstraction ([D9](DECISIONS.md)). Untrusted-input handling ships with the
first prompt, not as later hardening. Hecate stays fully usable with no LLM configured,
and an LLM never decides what crosses.

**Exit:** an LLM client can inspect a stuck Passage and request a crossing through MCP;
`hecate diagnose` explains a wedged Passage against a local Ollama model.

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

`pkg/ops` is the reason M6 comes where it does. Four consumers — CLI, MCP, API server,
UI — need the same operations, and the one thing that must not happen is four
implementations of "is this Bundle eligible" or "may this be approved". MCP and LLM
support (M6.5) are deliberately *after* it rather than early: they are thin adapters,
and built first they would each grow their own copy of those rules.

Fides (M4) and OTel (M5) come before both, because they are the differentiators and
because retrofitting tracing is far more expensive than building with it.

## 5. Supported versions

Stated, tested, and enforced in CI — not "latest".

| | Supported |
|---|---|
| Kubernetes | N-2, matching Flux |
| Flux | The three minor versions Flux itself supports |
| Hecate | The latest **two** minors |

Flux currently ships [v2.9 (June 2026)](https://fluxcd.io/blog/2026/06/flux-v2.9.0/),
maintains three minors at a time, and releases roughly every four months following the
Kubernetes cadence. We support two rather than three because the team is smaller and an
unhonoured support promise is worse than a modest one.

## 6. Following Flux upstream

Hecate's value depends entirely on reading Flux's status contract correctly, and Flux
moves — v2.9 removed two beta API versions outright. The mechanism below is ordered
**automation first, humans last**, because a tracking plan whose only mechanism is
"review it quarterly" has already failed.

1. **CI is the tracker.** The e2e job runs against all three Flux minors Flux supports,
   not just the newest. A Flux release that breaks us fails a build. This is the only
   mechanism that cannot be forgotten.
2. **Renovate watches `fluxcd/flux2`.** A new Flux release opens a PR bumping the e2e
   matrix. The PR going red *is* the notification.
3. **Status fixtures per Flux minor.** `pkg/flux` tests read real captured status output
   for each supported version, so a contract change surfaces as a unit-test failure in
   milliseconds rather than as a user's support ticket.
4. **Startup API discovery check.** Covers the gap CI cannot: a cluster running a Flux
   version we did not test. Warns rather than fails — refusing to start because Flux is
   newer than us would be worse than the problem.
5. **Quarterly upstream review**, aligned to Flux's cadence. Fixed checklist: new and
   removed API versions, deprecations, new extension points, roadmap deltas, support
   matrix refresh.
6. **Participate.** Watch `fluxcd/flux2` releases and discussions, and take gaps to
   Flux's RFC process rather than working around them privately.

The exact API versions and status fields we depend on are listed in
[ARCHITECTURE.md](ARCHITECTURE.md#flux-compatibility-surface). That list is the blast
radius of any Flux change.

## 7. Maintenance policy

| Policy | Commitment |
|---|---|
| Release cadence | A minor within ~1 month of each Flux minor (≈3/year); patches as needed |
| Supported versions | Latest two Hecate minors |
| API lifecycle | Mirrors Flux exactly: alpha deprecated on the next alpha/beta and removed after 3 months; beta deprecated on the next beta/stable; stable never removed within a major |
| Breaking changes | Majors only, announced one minor ahead, with documented migration |
| Security | `SECURITY.md`, private reporting, 90-day disclosure, patched across all supported minors |
| Dependencies | Renovate weekly and grouped; security updates immediately |
| Supply chain | cosign signatures and SBOM per release, with Hecate's own provenance recorded in Fides |

**Long-term direction.** Stay a thin, removable layer above Flux. Every capability Flux
or its ecosystem absorbs, we delete rather than duplicate — that is the maintenance
strategy, not a concession to it. Beacon will not grow monorepo decomposition because
ArtifactGenerator owns it ([D15](DECISIONS.md)); we will never ship a notifier because
notification-controller owns it ([D12](DECISIONS.md)). What stays ours is the promotion
model, the evidence gate, and the operator experience.

## 8. Testing

| Layer | Approach | Cluster? |
|---|---|---|
| `pkg/flux` | Table-driven fixtures | no |
| `pkg/health`, `pkg/passage` | Fake clients, scripted runners | no |
| `pkg/fides` | httptest against recorded responses | no |
| Controllers | envtest | no |
| End to end | kind + Flux + Flagger + a real git provider | yes |
| Flux compatibility | e2e matrix across all three supported Flux minors | yes |
| Providers | Nightly, one job each | yes |

**Rule: anything that can be tested without a cluster must be.** The current suite
runs in 0.2s; that is the bar. End-to-end tests catch what fixtures cannot — real Flux
status output — and are not a substitute for unit tests.

## 9. Release

- SemVer; `v0.x` until the CRD contract is stable.
- Images to GHCR, chart to an OCI registry.
- Signed with cosign, SBOM attached, and **each release's provenance recorded in
  Fides**. If we will not gate our own releases with it, we should not ask anyone else
  to.

## 10. Definition of done

- [ ] Tests, running without a cluster wherever possible
- [ ] `gofmt` and `go vet` clean
- [ ] Docs updated
- [ ] An `examples/` entry if it adds a step, checker, or API field
- [ ] A [decision record](DECISIONS.md) entry if it settles a non-obvious question
- [ ] OTel spans if it touches the Passage path
