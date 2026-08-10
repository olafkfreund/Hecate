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
  <span class="badge ok">spike: green</span>
  <span class="badge info">Apache 2.0</span>
  <span class="badge info">Go 1.26</span>
</div>

</div>

> *Hecate **Kleidouchos**, "the key-holder": goddess of keys, gates, thresholds and
> crossroads. She stands at the boundary and decides what may pass.*

---

## Why {#why}

Flux is very good at one thing: making a cluster match what is in git. It has no opinion
about **what should be in git next**.

So the moment you have more than one environment, you hit the same wall every Flux team
hits. How does the image that passed tests in `dev` get to `staging`? What is actually in
production right now, and who put it there? What stops it going to prod on a Friday, or
before the security scan finished?

Flux does not answer these. Nor does Argo CD. The difference is that **Argo CD users have
an answer** — [Kargo](https://github.com/akuity/kargo) — and Flux users do not.

<div class="note gap" markdown="1">
**The gap, precisely.** The [Flux ecosystem](https://fluxcd.io/ecosystem/) has UIs,
extensions, integrations and ancillary tools. It has **no promotion category at all.**
</div>

The two things people reach for instead don't close it:

- **[Flagger](https://flagger.app/)** is excellent, and it is *progressive delivery within
  one environment* — shifting traffic between two versions in one cluster. It does not
  move an artifact from dev to staging to prod.
- **[Flux Operator](https://fluxoperator.dev/) `ResourceSet`** gives you templated and
  ephemeral environments with genuinely strong input discovery. It has no Freight, no
  Stages, no gates, no approvals, and no answer to "how did this get to prod?"

So teams write the promotion logic themselves: a pile of `yq` in CI that nobody trusts and
one person understands.

<pre class="ascii"><code>                 within one environment        across environments
              ┌──────────────────────────┬──────────────────────────┐
  Argo CD     │  Argo Rollouts           │  Kargo                   │
              ├──────────────────────────┼──────────────────────────┤
  Flux        │  Flagger                 │  ── HECATE ──            │
              └──────────────────────────┴──────────────────────────┘</code></pre>

---

## How {#how}

Hecate models delivery the way you already think about it:

<div class="table-scroll" markdown="1">

| Concept | What it is |
|---|---|
| **Warehouse** | Watches your registries, charts and repos. Discovers new artifacts. |
| **Freight** | An immutable, content-addressed set of artifact versions — *this* commit plus *these* images. The unit that moves. |
| **Stage** | An environment. Requests Freight from a Warehouse or from an upstream Stage — that's what forms the pipeline. |
| **Promotion** | One execution of a Stage's steps against one piece of Freight. Auditable, replayable, abortable. |

</div>

A Stage looks like this:

```yaml
apiVersion: kargo.akuity.io/v1alpha1
kind: Stage
metadata:
  name: production
spec:
  requestedFreight:
    - origin: { kind: Warehouse, name: podinfo }
      sources: { stages: [staging] }     # only what already survived staging
  promotionTemplate:
    spec:
      steps:
        - uses: git-clone
          config: { repoURL: https://github.com/acme/deploy, checkout: [{ branch: main, path: ./repo }] }
        - uses: kustomize-set-image
          config: { path: ./repo/prod, images: [{ image: ghcr.io/acme/podinfo }] }
        - uses: git-commit
          as: commit
          config: { path: ./repo, message: "promote ${{ imageFrom('ghcr.io/acme/podinfo').Tag }} to prod" }
        - uses: git-push
          config: { path: ./repo }

        # ── Hecate's part ──────────────────────────────────────────────
        - uses: flux-wait
          config:
            resources:
              - { kind: Kustomization, name: podinfo, namespace: flux-system }
            expectedRevision: ${{ outputs.commit.commit }}
            failAfter: 15m
```

**Hecate never talks to Flux.** It writes to git; Flux syncs; Hecate reads
`Kustomization` and `HelmRelease` status back to know whether it worked. Git is the
rendezvous. That one rule is what keeps Flux authoritative and keeps Hecate removable —
uninstall it and you are left with working manifests.

### Honest health

Most naive Flux health checks are wrong in the same three ways. Ours are tested against
all three:

- **`Ready=True` is not enough.** If `observedGeneration` is behind `metadata.generation`,
  that Ready describes the *previous* revision. This is the classic false-green.
- **`Ready=False` is not failure.** Flux retries most failures forever, so a bare
  `Ready=False` never becomes terminal. We report *Progressing* until Flux sets
  `Stalled=True` or the failure outlives a deadline you set.
- **Suspended is not progressing.** A suspended resource will never reconcile. Reporting
  "still working" would hang a promotion forever waiting on a human who isn't coming.

---

## Evidence, not vibes {#fides}

This is the part no other promotion tool does.

A promotion **is** a change gate. [Fides](https://github.com/olafkfreund/fides) already
models exactly that, so the two line up with no impedance mismatch:

<div class="table-scroll" markdown="1">

| Hecate | Fides |
|---|---|
| Freight (immutable artifact set) | Trail |
| Image digest inside the Freight | Artifact (SHA256) |
| Stage | Environment |
| Verification result | Attestation |
| Approving a Promotion | `fides approve` — segregation of duties |
| *May this go to production?* | `GET /api/v1/trails/{id}/change-gate` → verdict + risk score |

</div>

<div class="note win" markdown="1">
The result: **"can this ship to prod?" is answered by evidence, not by whoever holds merge
rights** — and every promotion leaves a tamper-evident record that maps onto SOC 2,
ISO 27001, NIST 800-53, PCI-DSS, DORA and SOX controls.
</div>

### Traced end to end

Every promotion is an OpenTelemetry **trace**; every step a **span**. Trace context is
propagated into git commit trailers, so one trace can span your CI run, the promotion, the
Flux reconciliation, and the first request that hit the new version.

DORA metrics — lead time, deployment frequency, change-failure rate, MTTR — fall out of
that trace data instead of being a bolt-on subsystem.

---

## What we are building on

Kargo is Apache-2.0, ~193k lines of mature Go, and its coupling to Argo CD turns out to be
**two promotion steps, one health checker, and Argo Rollouts verification** — 37 of roughly
800 non-test source files.

So Hecate does not reimplement it. We wrote the Flux adapter and inherit the rest: the
pipeline model, 34 engine-neutral promotion steps, the API server, RBAC, SSO, sharding,
GC, every git provider and every cloud registry credential helper.

<div class="note" markdown="1">
**The spike is green.** ~370 lines of adapter, 10 tests, no cluster required, building
against Kargo v1.11.1. It also surfaced three fixable blockers — two of which are upstream
PRs we intend to contribute back. The full write-up, including what broke and why, is in
[`docs/SPIKE-RESULTS.md`](https://github.com/olafkfreund/Hecate/blob/main/docs/SPIKE-RESULTS.md).
</div>

We reuse rather than rebuild wherever the ecosystem already solved something:

- **Flux Operator** `ResourceSetInputProvider` for artifact discovery — it already covers
  GitHub, GitLab, Azure DevOps, AWS CodeCommit, Gitea, OCI, ACR, ECR and GAR.
- **Flagger** for in-environment verification.
- **Kargo** for the promotion engine.
- **Fides** for evidence.

Hecate is not a Flux replacement, a Flagger replacement, or a Flux Operator competitor. It
is the layer above all three that none of them provide.

---

## Roadmap {#roadmap}

<div class="table-scroll" markdown="1">

| | Milestone | What lands |
|---|---|---|
| ✅ | **M0 · Spike** | Flux health checker + `flux-wait`, proven against Kargo v1.11.1 |
| | **M1 · First binary** | Composed control plane, Helm chart, real promotion on a kind cluster in CI |
| | **M2 · Flux depth** | `flux-reconcile`, Flagger verification, remote clusters, `ResourceSetInputProvider` |
| | **M3 · Fides** | Change gate before, attestation after, approvals mapped to SoD |
| | **M4 · OpenTelemetry** | Promotion traces, git-trailer context propagation, DORA |
| | **M5 · CLI** | Full surface, `json`/`yaml`/`table` output, documented exit codes |
| | **M6 · UI** | Pipeline graph, Freight timeline, one-click promote, evidence panel |
| | **M7 · Providers** | Every major git provider and cloud registry verified in CI, not claimed |
| | **M8 · v1.0** | SSO, RBAC, upgrade path, security review |

</div>

The CLI is the product; the UI is the adoption multiplier. Gates return documented exit
codes so they compose in pipelines.

---

## Status

Pre-alpha. The spike is done and green; there is no installable release yet.

If you run Flux across more than one environment and have opinions about how promotion
*should* work, [open an issue](https://github.com/olafkfreund/Hecate/issues) — this is
exactly the moment when that input changes the design.

- 📄 [Product plan](https://github.com/olafkfreund/Hecate/blob/main/docs/PRODUCT-PLAN.md)
- 🔧 [Development plan](https://github.com/olafkfreund/Hecate/blob/main/docs/DEVELOPMENT-PLAN.md)
- 🧪 [Spike results](https://github.com/olafkfreund/Hecate/blob/main/docs/SPIKE-RESULTS.md)
