---
layout: default
title: Hecate
description: The promotion and release-orchestration layer for FluxCD — with compliance evidence and OpenTelemetry tracing built in.
---

<div class="hero" markdown="1">

# Hecate

<p class="tagline">The promotion and release-orchestration layer for <strong>FluxCD</strong>.<br>
The gate between your environments — with evidence.</p>

<div class="badges">
  <span class="badge warn">status: pre-alpha</span>
  <span class="badge ok">33 tests green</span>
  <span class="badge info">Apache 2.0</span>
  <span class="badge info">Go 1.26</span>
</div>

</div>

> *Hecate **Kleidouchos**, "the key-holder": goddess of keys, gates, thresholds and
> crossroads. She stands at the boundary and decides what may pass.*

---

## Why {#why}

Flux is very good at one thing: making a cluster match what is in git. It has no
opinion about **what should be in git next**.

So the moment you have more than one environment, you hit the wall every Flux team
hits. How does the image that passed tests in `dev` get to `staging`? What is actually
in production right now, and who put it there? What stops it going to prod on a
Friday, or before the security scan finished?

<div class="note gap" markdown="1">
**The gap, precisely.** The [Flux ecosystem](https://fluxcd.io/ecosystem/) has UIs,
extensions, integrations and ancillary tools. It has **no promotion category at all.**
</div>

The two things people reach for don't close it:

- **[Flagger](https://flagger.app/)** is excellent, and it is *progressive delivery
  within one environment* — shifting traffic between two versions in one cluster. It
  does not move an artifact from dev to staging to prod.
- **[Flux Operator](https://fluxoperator.dev/) `ResourceSet`** gives you templated and
  ephemeral environments with genuinely strong input discovery. It has no promotion
  model: no immutable release unit, no thresholds, no approvals, no history.

So teams write the promotion logic themselves. Flux's own docs still recommend
[doing it with GitHub Actions](https://fluxcd.io/flux/use-cases/gh-actions-helm-promotion/)
— which means **our real competition is a workflow file**, not another product.

That workflow has no memory of what is in which environment, pushes and hopes rather
than waiting for Flux to converge, treats "who can merge" as the approval model, leaves
no queryable history, and cannot answer *why is this stuck?*. Those gaps are the
product.

<pre class="ascii"><code>                 within one environment        across environments
              ┌──────────────────────────┬──────────────────────────┐
  Argo CD     │  Argo Rollouts           │  (well served)           │
              ├──────────────────────────┼──────────────────────────┤
  Flux        │  Flagger                 │  ── HECATE ──            │
              └──────────────────────────┴──────────────────────────┘</code></pre>

---

## How {#how}

Four resources. That is the whole API.

<div class="table-scroll" markdown="1">

| | |
|---|---|
| **Beacon** | Watches your registries, charts and repos. Emits a Bundle when something new appears. |
| **Bundle** | An immutable, content-addressed set of artifact versions — *this* commit plus *these* images. The unit that moves. |
| **Gate** | An environment, and the threshold a Bundle must cross to enter it. |
| **Passage** | One attempt to move one Bundle through one Gate. Auditable, resumable, abortable. |

</div>

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
        with: { repo: https://github.com/acme/deploy, branch: main }
      - uses: set-image
        with: { path: envs/prod, image: ghcr.io/acme/podinfo }
      - uses: git-commit
        as: commit
        with: { message: "promote podinfo to prod" }
      - uses: git-push

      - uses: flux-wait
        with:
          resources:
            - { kind: Kustomization, name: podinfo, namespace: flux-system }
          expectedRevision: ${{ steps.commit.sha }}
          failAfter: 15m
```

**There is no pipeline object.** The graph is implied by what each Gate admits — a
separate pipeline resource would be a second source of truth that drifts out of sync
with the Gates it describes.

**Hecate never talks to Flux.** It writes to git; Flux syncs; Hecate reads
`Kustomization` and `HelmRelease` status back to know whether it worked. Git is the
rendezvous. That one rule keeps Flux authoritative and keeps Hecate removable —
uninstall it and you are left with working manifests.

### Honest health

Most naive Flux health checks are wrong in the same three ways. Ours are tested
against all three:

- **`Ready=True` is not enough.** If `observedGeneration` is behind
  `metadata.generation`, that Ready describes the *previous* revision. The classic
  false-green.
- **`Ready=False` is not failure.** Flux retries most failures forever, so a bare
  `Ready=False` never becomes terminal. We report *Progressing* until Flux sets
  `Stalled=True` or the failure outlives a deadline you set.
- **Suspended is not progressing.** A suspended resource will never reconcile.
  Reporting "still working" would hang a Passage forever waiting on a human who isn't
  coming.

There is a fourth, at the framework level: a health check naming a checker that isn't
registered is **reported**, not skipped. Silently ignoring a check you asked for makes
a Gate look healthier than it is.

---

## Evidence, not vibes {#fides}

This is the part no other promotion tool does.

A crossing **is** a change gate. [Fides](https://github.com/olafkfreund/fides) already
models exactly that, so the two line up with no impedance mismatch:

<div class="table-scroll" markdown="1">

| Hecate | Fides |
|---|---|
| Bundle | Trail |
| Image digest inside the Bundle | Artifact (SHA256) |
| Gate | Environment |
| Verification outcome | Attestation |
| Approving a Passage | `fides approve` — segregation of duties |
| *May this cross?* | four gates: artifact policy, environment policy, digest allowlist, change-gate verdict + risk |

</div>

<div class="note win" markdown="1">
The result: **"can this ship to prod?" is answered by evidence, not by whoever holds
merge rights** — and every Passage leaves a tamper-evident record that maps onto
SOC 2, ISO 27001, NIST 800-53, PCI-DSS, DORA and SOX controls.
</div>

### Traced end to end

Every Passage is an OpenTelemetry **trace**; every step a **span**. Trace context is
propagated into git commit trailers, so one trace can span your CI run, the crossing,
the Flux reconciliation, and the first request that hit the new version.
`traceID` is a first-class field on Passage status, not an annotation bolted on later.

DORA metrics — lead time, deployment frequency, change-failure rate, MTTR — fall out
of that trace data instead of being a separate subsystem.

---

## Built from scratch, on purpose

Hecate is its own implementation: its own API, vocabulary, engine and step library,
with no third-party promotion engine in the dependency graph.

We prototyped the alternative — adapting an existing engine — and it worked. We chose
not to ship it. An adapter makes the roadmap someone else's to set, and the features
we care about most would have been bolted onto a model designed around different
assumptions. Owning the model is the point; the engine is the cheap part.

<div class="note" markdown="1">
**What that costs, stated plainly.** We reimplement the step library, control plane
and provider integrations. It is smaller than it sounds: most of the value in that
layer is *wiring*, not original algorithms — `go-git`, the Helm SDK, the kustomize API
and `go-containerregistry` do the actual work, and we call them directly.

The reasoning is recorded in
[`docs/DECISIONS.md`](https://github.com/olafkfreund/Hecate/blob/main/docs/DECISIONS.md).
</div>

We still reuse the ecosystem wherever it already solved something: **Flux Operator**
`ResourceSetInputProvider` for artifact discovery, **Flagger** for in-environment
verification, **Fides** for evidence.

Hecate is not a Flux replacement, a Flagger replacement, or a Flux Operator
competitor. It is the layer above all three that none of them provide.

---

## Roadmap {#roadmap}

<div class="table-scroll" markdown="1">

| | Milestone | What lands |
|---|---|---|
| ✅ | **M0 · Foundations** | API, status evaluation, health framework, step engine — 33 tests |
| | **M1 · Control plane** | Controllers, Helm chart, a real crossing on kind in CI |
| | **M2 · Step library** | git, render, OCI, expressions, per-step schemas |
| | **M3 · Providers** | Git hosts and registries, verified in CI rather than claimed |
| | **M4 · Evidence** | Change gate, attestations, approvals mapped to segregation of duties |
| | **M5 · OpenTelemetry** | Passage traces, git-trailer context propagation, DORA |
| | **M6 · CLI** | One shared operations layer, then the CLI: `json`/`yaml`/`table`, documented exit codes |
| | **M6.5 · MCP + LLM** | An MCP server so agents can drive Hecate; optional local LLM for diagnosis |
| | **M7 · API server** | SSO, RBAC — the UI's backend |
| | **M8 · UI** | Pipeline graph, Bundle timeline, one-click crossing, evidence panel |
| | **M9 · v1.0** | CRD stability, upgrade path, security review |

</div>

The CLI lands before the UI on purpose. The CLI proves the API surface is coherent and
it is what early adopters actually use; a UI built before the model settles is rework.

The CLI, MCP server, API server and UI all call **one operations layer**. Four
implementations of "is this Bundle eligible" or "may this be approved" is the failure
mode worth designing against. An LLM can inspect and request; it never decides what
crosses — that is what evidence gates are for.

---

## Status

Pre-alpha. The foundations are built and tested; there is no installable release yet.

If you run Flux across more than one environment and have opinions about how promotion
*should* work, [open an issue](https://github.com/olafkfreund/Hecate/issues) — this is
exactly the moment when that input changes the design.

- 🏛 [Architecture](https://github.com/olafkfreund/Hecate/blob/main/docs/ARCHITECTURE.md)
- 🧩 [Step reference](https://github.com/olafkfreund/Hecate/blob/main/docs/STEPS.md)
- 📄 [Product plan](https://github.com/olafkfreund/Hecate/blob/main/docs/PRODUCT-PLAN.md)
- 🔧 [Development plan](https://github.com/olafkfreund/Hecate/blob/main/docs/DEVELOPMENT-PLAN.md)
- ⚖️ [Decision record](https://github.com/olafkfreund/Hecate/blob/main/docs/DECISIONS.md)
