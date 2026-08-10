# Hecate — Product Plan

> *Hecate **Kleidouchos**, "the key-holder": goddess of keys, gates, thresholds and
> crossroads. She stands at the boundary and decides what may pass.*

**One line:** Hecate is the promotion and release-orchestration layer for FluxCD —
the gate between your environments, with compliance evidence built in.

---

## 1. The problem

Flux is very good at one thing: making a cluster match what is in git. It has no
opinion about **what should be in git next**.

The moment you have more than one environment, that gap becomes yours to fill. How
does the image that passed tests in dev reach staging? What is in production right
now, and who put it there? What stops it shipping on a Friday, or before the security
scan finished?

The [Flux ecosystem](https://fluxcd.io/ecosystem/) has UIs, extensions, integrations
and ancillary tools. It has **no promotion category at all**. So teams write the logic
themselves: a pile of `yq` in CI that nobody trusts and one person understands.

The two things people reach for do not close it:

- **Flagger** is excellent, and it is progressive delivery *within* one environment —
  shifting traffic between two versions in one cluster. It does not move an artifact
  from dev to staging to prod.
- **Flux Operator `ResourceSet`** gives templated and ephemeral environments with
  genuinely strong input discovery. It has no promotion model: no immutable release
  unit, no thresholds, no approvals, no history.

## 2. Who it is for

| Persona | Needs | Gets |
|---|---|---|
| **Platform engineer** running Flux for N teams | Promotion without bespoke CI per team | Declarative pipelines, self-service, one control plane |
| **Application team** shipping dev→staging→prod | To see where their change is and move it safely | Pipeline view, one-click crossing, honest health |
| **SRE / release manager** | Windows, approvals, rollback, blast-radius control | Promotion windows, thresholds, verification |
| **Compliance / audit** | Evidence controls were enforced, not just claimed | Evidence-gated crossings and a record per Passage |

The wedge persona is the platform engineer who already runs Flux and has concluded
that every promotion tool worth having assumes a different delivery engine.

## 3. Positioning

```
                 within one environment        across environments
              ┌──────────────────────────┬──────────────────────────┐
  Argo CD     │  Argo Rollouts           │  (well served)           │
              ├──────────────────────────┼──────────────────────────┤
  Flux        │  Flagger                 │  ── HECATE ──            │
              └──────────────────────────┴──────────────────────────┘
```

Hecate sits **above** Flux and cooperates with the ecosystem rather than competing
with it:

- **Flux** is the delivery engine. Hecate writes rendered state into git or OCI; Flux
  syncs it; Hecate reads resource status back as health. The rendezvous is the source
  of truth, never a direct call.
- **Flagger** is the verification engine for in-environment rollout. A Gate can gate
  on Flagger `Canary` outcome.
- **Flux Operator** owns Flux lifecycle and input discovery. Hecate consumes
  `ResourceSetInputProvider` rather than reimplementing artifact discovery.

**Ecosystem slot:** a new category on fluxcd.io/ecosystem — *promotion and release
orchestration*.

### 3.1 Who we are actually competing with

Not Flagger, and not Flux Operator. **A GitHub Actions workflow file.**

Flux's own documentation still recommends
[promoting Helm releases with GitHub Actions](https://fluxcd.io/flux/use-cases/gh-actions-helm-promotion/).
That is the incumbent: free, already installed, understood by everyone on the team, and
genuinely good enough for a single team with two environments.

This is good news twice over. The gap is unfilled by any product, and Flux's stated
best practice — *promotion should be a Git operation, not a script that modifies a
running cluster* — is [D3](DECISIONS.md) almost word for word. We are the productised
version of upstream's own advice rather than an argument with it.

But it sets a hard bar: **Hecate must be obviously better than fifty lines of YAML on
first contact.** What that workflow cannot do, and what every feature should be measured
against:

| A promotion workflow | Hecate |
|---|---|
| Has no memory — no idea what is in which environment | Gate status answers it directly |
| Pushes and hopes | Waits for Flux to actually converge on the pushed revision |
| Cannot gate on health or verification | Refuses to admit an unverified Bundle |
| Approval is "who can merge" | Approval is a distinct right, with segregation of duties |
| Leaves no queryable history | Every crossing is an immutable, auditable record |
| Cannot answer "why is this stuck?" | It is a first-class question |
| One workflow per repo, copy-pasted N times | One control plane for N teams |
| Fails silently when Flux does not converge | Honest health, with a deadline |

Any Hecate feature that does not widen one of those rows is decoration.

## 4. What makes Hecate different

A working promotion model is the price of entry, not the product. Three things are
ours.

### 4.1 Evidence-gated crossings

A promotion is structurally a change gate, and
[Fides](https://github.com/olafkfreund/fides) already models one — so the two line up
with no impedance mismatch:

| Hecate | Fides |
|---|---|
| Bundle | Trail |
| Image digest inside a Bundle | Artifact (SHA256) |
| Gate | Environment |
| Verification outcome | Attestation |
| Approving a Passage | `fides approve` — segregation of duties |
| *May this cross?* | four gates, below |

Fides exposes **four** gates with distinct exit codes, not one — and two are
environment-scoped, which maps exactly onto a Gate:

| Fides call | Hecate use |
|---|---|
| `assert --sha256 --policy` | policy over the Bundle's image digests |
| `policy check --env --trail` | the Gate's own environment policy |
| `allowlist check --env --sha` | is this exact image approved for this Gate |
| `change-gate --trail` | verdict plus a 0–100 risk score |

A Gate chooses which apply, so dev can run none and production can run all four.

The result: **"can this ship to prod?" is answered by evidence, not by whoever holds
merge rights** — and every Passage leaves a tamper-evident record mapping onto SOC 2,
ISO 27001, NIST 800-53, PCI-DSS, DORA and SOX controls.

No GitOps promotion tool does this. It is the single strongest reason to pick Hecate.

### 4.2 OpenTelemetry-native

Not "we export some Prometheus metrics." Every Passage is a **trace**, every step a
**span**, with trace context propagated into git commit trailers so one trace spans
the CI run, the crossing, the Flux reconciliation, and the first request that hit the
new version.

`PassageStatus.traceID` is a first-class API field, not an annotation bolted on later.

DORA metrics fall out of that trace data rather than being a separate subsystem.

### 4.3 A UI and CLI worth using

- **UI:** Next.js 16 + React 19 + Tailwind v4 + lucide-react + recharts + next-themes
  — the Fides portal stack, so the two products feel like one platform.
- **CLI:** the primary interface, not an afterthought. Every UI action has a CLI
  equivalent, `--output json|yaml|table` everywhere, and gates return **documented
  exit codes** so they compose in pipelines.

## 5. Product principles

1. **Git is the rendezvous.** Hecate never talks to Flux and never applies to a
   cluster. This keeps Flux authoritative and Hecate removable.
2. **Honest health.** A crossing that reports success while the deploy is wedged is
   worse than no tool. Stale `observedGeneration`, suspended resources, unregistered
   checkers and infinite Flux retries are all handled explicitly.
3. **History is immutable.** A Passage is never reused and a Bundle is never edited.
   A second attempt is a second Passage. That is what makes the record worth trusting.
4. **The CLI is the product.** If it cannot be scripted, it is not done.
5. **No lock-in.** Uninstalling Hecate leaves working Flux manifests in git.
6. **Fewer resources.** Four kinds, no pipeline object. Adding a resource later is
   easy; removing one is not.

## 6. Scope

### v0.1 — "it crosses"
Beacon, Bundle, Gate, Passage · controller · step engine · `flux-wait`,
`flux-reconcile`, git and rendering steps · CLI `get` / `cross` / `approve` · Helm
chart · OTel traces.

### v0.2 — "it gates"
Fides change gate before crossing · attestation after · approvals mapped to Fides SoD
· Flagger verification · promotion windows.

### v0.3 — "it is pleasant"
Hecate UI: pipeline graph, Bundle timeline, one-click crossing, approval queue,
evidence panel. Full OTel/DORA dashboards. `ResourceSetInputProvider` as a Beacon
source.

### v1.0 — "it is trusted"
Multi-cluster · every major git provider and cloud registry verified in CI · SSO ·
RBAC · documented upgrade path · production references.

### Non-goals

- Replacing Flux, Flagger or Flux Operator.
- Applying manifests to clusters directly. Ever.
- Supporting Argo CD. That space is well served.
- A hosted SaaS offering before v1.

## 7. Success metrics

| Horizon | Metric |
|---|---|
| 3 months | Installable v0.1; 3 external teams running it |
| 6 months | Listed on fluxcd.io/ecosystem; evidence gate used in a real audit |
| 12 months | 10 production references; a Passage record cited in a compliance report |

## 8. Key risks

| Risk | Severity | Response |
|---|---|---|
| **Scope.** Independence means building the control plane, step library, providers and UI ourselves | **High** | Sequence ruthlessly. Ship CLI-only through v0.2 — the UI is an adoption multiplier, not a prerequisite. Lean on `go-git`, the Helm SDK and the kustomize API rather than writing that layer. |
| ControlPlane ships promotion in Flux Operator | Medium | The likeliest real competitor. Differentiate on evidence and depth of model; cooperate on discovery rather than duplicating it. |
| Adoption: a new vocabulary raises the learning curve | Medium | One glossary, used consistently everywhere. Four resources is a small surface to learn. |
| Flux status contract changes | Low | Stable across v1/v2; we read it as unstructured and test against fixtures. |
| `hecate.dev` unregistered | Low, but blocking later | Register it. Tracked as an issue. |
