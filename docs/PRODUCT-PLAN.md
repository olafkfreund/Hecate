# Hecate — Product Plan

> *Hecate Kleidouchos, "the key-holder": goddess of keys, gates, thresholds and
> crossroads. She stands at the boundary and decides what may pass.*

**One line:** Hecate is the promotion and release-orchestration layer for FluxCD —
the gate between environments, with compliance evidence built in.

---

## 1. The problem

Neither Flux nor Argo CD solves **cross-environment promotion**. They reconcile one
declared state onto one set of targets; they have no concept of "this version is in
staging and should move to production once it proves itself."

Argo CD users have an answer: **Kargo**. Flux users have three bad options:

1. **Hand-rolled CI** — a pile of `yq` in GitHub Actions that nobody trusts and one
   person understands.
2. **Flagger** — excellent, but it is progressive delivery *within* one environment.
   It shifts traffic between two versions in one cluster; it does not move an artifact
   from dev to staging to prod.
3. **Flux Operator `ResourceSet`** — templated and ephemeral environments with strong
   input discovery. It has no Freight, no Stages, no gates, no approvals, no answer to
   "what is in prod right now, and how did it get there?"

The gap is real and the Flux ecosystem page has no category for it.

## 2. Who it is for

| Persona | What they need | What they get |
|---|---|---|
| **Platform engineer** running Flux for N teams | Promotion without writing bespoke CI per team | Declarative pipelines, self-service, one control plane |
| **Application team** shipping to dev→staging→prod | To see where their change is and promote it safely | Pipeline UI, one-click promote, honest health |
| **SRE / release manager** | Windows, approvals, rollback, blast-radius control | Promotion windows, gates, verification, per-Target fan-out |
| **Compliance / audit** | Evidence that controls were enforced, not just claimed | Fides-backed change gates and an attestation per promotion |

The wedge persona is the platform engineer who already runs Flux, has been told
"just use Kargo", and discovered Kargo's health model assumes Argo CD.

## 3. Positioning

```
                 within one environment        across environments
              ┌──────────────────────────┬──────────────────────────┐
  Argo CD     │  Argo Rollouts           │  Kargo                   │
              ├──────────────────────────┼──────────────────────────┤
  Flux        │  Flagger                 │  ── HECATE ──            │
              └──────────────────────────┴──────────────────────────┘
```

Hecate is **not** a Flux replacement, a Flagger replacement, or a Flux Operator
competitor. It sits above Flux and cooperates with all three:

- **Flux** is the delivery engine. Hecate writes rendered state into git or OCI;
  Flux syncs it; Hecate reads `Kustomization`/`HelmRelease` status back as health.
  The rendezvous is the source of truth, never a direct call.
- **Flagger** is the verification engine for in-environment rollout. A Hecate Stage can
  gate on Flagger `Canary` status the way Kargo gates on Argo Rollouts `AnalysisRun`.
- **Flux Operator** owns Flux lifecycle and input discovery. Hecate consumes
  `ResourceSetInputProvider` rather than reimplementing artifact discovery.

**Ecosystem slot:** a new category on fluxcd.io/ecosystem — *promotion and release
orchestration*.

## 4. What makes Hecate different from "Kargo but Flux"

Feature parity with Kargo is the price of entry, not the product. Three things are ours.

### 4.1 Compliance-gated promotion (Fides)

A promotion is structurally a change gate, and Fides already models it:

| Hecate | Fides |
|---|---|
| Freight (immutable artifact set) | Trail |
| Container image digest in Freight | Artifact (SHA256) |
| Stage | Environment / logical-env |
| Verification result | Attestation |
| Manual approval of a Promotion | `fides approve` (segregation of duties) |
| Promotion allowed? | `GET /api/v1/trails/{id}/change-gate` → approve/hold + 0–100 risk |

This yields a promotion pipeline where **"can this go to prod?" is answered by evidence,
not by whoever has merge rights** — and every promotion leaves a tamper-evident record.
No GitOps promotion tool does this today. It is the reason to choose Hecate over waiting
for Kargo 2.0 to become engine-agnostic.

### 4.2 OpenTelemetry-native

Not "we export some Prometheus metrics." Every promotion is a **trace**, every step a
**span**, with trace context propagated into git commit trailers so a deployment can be
correlated end-to-end from CI through Flux reconciliation to the first request that hit
the new version. Fides itself only carries OTel as an indirect dependency, so Hecate
leads here and Fides can adopt the same conventions.

DORA metrics (lead time, deployment frequency, change-failure rate, MTTR) fall out of the
trace data rather than being a separate subsystem.

### 4.3 A UI and CLI people actually want to use

- **UI:** Next.js 16 + React 19 + Tailwind v4 + lucide-react + recharts + next-themes —
  the Fides portal stack, so the two products feel like one platform. Kargo's Ant Design
  UI is not reused.
- **CLI:** the primary interface, not an afterthought. Every UI action has a CLI
  equivalent, output is `--output json|yaml|table` everywhere, and gates return
  **documented exit codes** so they compose in pipelines — the contract Fides already
  proved works.

## 5. Product principles

1. **Git is the rendezvous.** Hecate never talks to Flux directly and never applies to a
   cluster. It writes to a source of truth and reads status back. This is what keeps
   Flux authoritative and Hecate replaceable.
2. **Honest health.** A promotion that reports success when the deploy is wedged is worse
   than no tool. Stale `observedGeneration`, suspended resources, and infinite Flux
   retries are all handled explicitly (see `docs/SPIKE-RESULTS.md`).
3. **The CLI is the product.** If it cannot be done from a terminal and scripted in CI,
   it is not done.
4. **Reuse before build.** Kargo for the engine, Flux Operator for discovery, Flagger for
   verification, Fides for evidence. We write adapters and the things nobody else has.
5. **No lock-in.** Uninstalling Hecate leaves working Flux manifests in git.

## 6. Scope

### v0.1 — "it promotes" (the wedge)
Flux health checker · `flux-wait` / `flux-reconcile` steps · Stage/Freight/Promotion via
Kargo · CLI `get`/`promote`/`approve` · Helm chart · OTel traces on promotions.

### v0.2 — "it gates"
Fides change-gate integration · promotion approvals mapped to Fides SoD · attestation
per promotion · Flagger `Canary` verification · promotion windows.

### v0.3 — "it is pleasant"
Hecate UI (pipeline graph, Freight timeline, one-click promote) · full OTel/DORA
dashboards · `ResourceSetInputProvider` as a Warehouse source.

### v1.0 — "it is trusted"
Multi-cluster / remote Flux · all major git providers and cloud registries verified in CI
· SSO · RBAC · documented upgrade path · production references.

### Non-goals

- Replacing Flux, Flagger or Flux Operator.
- Applying manifests to clusters directly. Ever.
- Supporting Argo CD. That is Kargo's job and it does it well.
- A hosted SaaS offering in v1.

## 7. Success metrics

| Horizon | Metric |
|---|---|
| 3 months | Listed on fluxcd.io/ecosystem; 3 external teams running v0.1 |
| 6 months | Fides gate used in a real audit; 25+ GitHub stars from Flux community; both upstream Kargo PRs merged |
| 12 months | 10 production references; promotion→attestation flow cited in a compliance report |

## 8. Key risks

| Risk | Severity | Response |
|---|---|---|
| **Kargo 2.0 becomes engine-agnostic and obsoletes the adapter** | High | This is the *goal*, not the threat. Path A means we ride it. Our durable value is Fides + OTel + UI, none of which Kargo 2.0 provides. Keep `pkg/flux` Kargo-free so we can stand alone. |
| Upstream PRs (module tags, generic client) rejected | Medium | Both have working local workarounds already proven in the spike. |
| Flux status contract changes | Low | Contract is stable across v1/v2; we test against fixtures and pin nothing. |
| UI is the long pole | Medium | Ship v0.1 and v0.2 CLI-only. The CLI is the product; the UI is the adoption multiplier. |
| ControlPlane ships promotion in Flux Operator | Medium | Likeliest real competitor. Differentiate on Fides evidence and depth of the promotion model, and cooperate rather than duplicate discovery. |
