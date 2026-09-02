# Hecate — Architecture

## The model

Four resources. That is the whole API.

```
   registries, charts, repos
              │
              ▼
        ┌───────────┐
        │  BEACON   │  watches sources, emits a Bundle when something new appears
        └─────┬─────┘
              │
              ▼
        ┌───────────┐
        │  BUNDLE   │  immutable, content-addressed set of artifact versions
        └─────┬─────┘
              │  admitted by
              ▼
        ┌───────────┐        ┌───────────┐        ┌───────────┐
        │   GATE    │───────▶│   GATE    │───────▶│   GATE    │
        │    dev    │        │  staging  │        │   prod    │
        └─────┬─────┘        └───────────┘        └───────────┘
              │ crossing performed by
              ▼
        ┌───────────┐
        │  PASSAGE  │  one attempt to move one Bundle through one Gate
        └─────┬─────┘
              │ steps write to
              ▼
        ┌───────────┐   the rendezvous
        │ git / OCI │◀──────────────────┐
        └─────┬─────┘                   │
              │ synced by               │ status read back by flux-wait
              ▼                         │
        ┌───────────┐                   │
        │   FLUX    │───────────────────┘
        └───────────┘
```

### Beacon

Watches artifact sources — container repositories, Helm charts, git — and emits a
**Bundle** when it sees a new combination worth promoting.

One Beacon per set of artifacts that move *together*. Two services released on
independent schedules want two Beacons, otherwise a change to one drags the other
through the pipeline with it.

### Bundle

An immutable, content-addressed set of artifact versions: one git commit plus two
container images, say. The unit that moves.

The digest is computed over the artifacts, **sorted and canonicalised**, so the same
release discovered in a different order is the same Bundle. Image digests are
preferred over tags, so re-pointing a mutable tag at the same image does not
manufacture a new Bundle. Both of those are the difference between promoting
something once and promoting it twice.

A Bundle is never edited. Its status accumulates a record of which Gates it has
cleared, which blocked it, and why — the "how did this get to prod?" answer.

### Gate

An environment, and the threshold a Bundle must cross to enter it.

**There is no pipeline object.** The graph is implied by what each Gate admits:

```yaml
spec:
  admits:
    - from: { beacon: podinfo }
      after: [staging]      # this Gate is downstream of staging
```

A separate pipeline resource would be a second source of truth that drifts out of
sync with the Gates it describes. Deriving the graph means it cannot.

A Gate declares:

- `admits` — which Bundles may cross, and what must have happened first
- `passage` — the steps that move an admitted Bundle in
- `watch` — continuous health checks
- `verify` — one-shot evidence gathering after crossing
- `windows` — when crossings may start
- `auto` — whether eligible Bundles cross without being asked

### Passage

One attempt to move one Bundle through one Gate. Created, run to completion, then a
permanent record. A second attempt is a second Passage — nothing is overwritten, which
is what makes the history worth trusting.

The step list is **copied** from the Gate at creation time rather than referenced.
Editing a Gate must not retroactively change what an in-flight or completed Passage
did.

When the Gate names a Fides environment, a finished Passage is also recorded on the
artifact's trail as a `promotion` attestation — the Bundle, the Gate, the actor, every
step including the skipped ones, and the outcome. Chained into Fides' tamper-evident
hash chain, that turns a crossing into evidence rather than a log line in a cluster
that will be rebuilt next quarter, and it is what `hecate verify` checks. Written once,
never retried, and a failure to write it never fails the promotion — see
[D47](DECISIONS.md).

## Health is not verification

Two different questions, deliberately kept apart:

|  | Health | Verification |
|---|---|---|
| Asks | "is it running right now?" | "did it work?" |
| Shape | continuous | one-shot, after crossing |
| Example | Flux `Kustomization` is Ready at the pushed revision | the canary analysis passed |

A Gate can be healthy and unverified (deployed, tests not yet run), or verified and
unhealthy (it passed, then fell over an hour later). Conflating them is how you
promote out of an environment that is currently on fire.

## The rendezvous

**No deployed state reaches Flux except through git.** A Passage writes rendered state
into git or OCI; Flux syncs it; `flux-wait` reads Flux's own resource status to learn
whether the write took effect.

Two things Hecate writes do land directly on a Flux object, and neither carries
content: `flux-reconcile` sets the doorbell annotation ([D26](DECISIONS.md)), and an
operator can suspend, resume or reconcile a watched resource from the UI
([D66](DECISIONS.md)). Both change when or whether Flux acts; what it applies still
only ever comes from git.

That single rule buys three things: Flux stays authoritative, Hecate is removable
(uninstall it and working manifests remain in git), and the boundary is a data format
rather than an API contract, so a different delivery engine is a new step and a new
checker, not a redesign.

## Honest health

Most naive Flux health checks are wrong in the same three ways. `pkg/flux` handles all
three, and there is a test for each.

**Stale status is the false-green.** `Ready=True` with `observedGeneration` behind
`metadata.generation` describes the *previous* revision. Checked before conditions.

**`Ready=False` is not failure.** Flux retries most failures forever, so a bare
`Ready=False` never becomes terminal on its own. Reported Progressing until either
`Stalled=True` (Flux has given up) or the condition has been failing longer than
`failAfter` — default 10 minutes, tunable per Gate. Without that deadline a wedged
deploy reports "still working" indefinitely.

**Suspended is not Progressing.** `spec.suspend: true` means Flux will never
reconcile. Reporting Progressing hangs a Passage forever waiting on a human who is not
coming. Reported Unknown, with the reason.

There is a fourth, at the framework level: a health check naming an **unregistered
checker** is reported as Unknown rather than skipped. Silently ignoring a check the
user asked for makes a Gate look healthier than it is.

## Reconciling on demand

A Beacon polls on its interval, and a Gate re-evaluates on its own. Either can
be asked to act now:

```console
$ kubectl annotate beacon/app reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
```

This is Flux's annotation, deliberately: the same operation on an adjacent
object should not need a second convention. The value is opaque and comes back
in `status.lastHandledReconcileAt`, so a caller can wait for its own request:

```console
$ kubectl wait beacon/app --for=jsonpath='{.status.lastHandledReconcileAt}'=1755000000
```

The intended caller is CI. A job that has just pushed an image can poke the
Beacon in the same step it already authenticated for, which takes discovery
latency from minutes to seconds — most of the perceived speed difference against
a promotion script.

**There is no webhook server**, and that is the point: the API server is already
an authenticated, audited, RBAC-controlled endpoint, so a second one would be a
second authorisation model to get wrong. A registry posting a webhook with no
cluster credentials is not covered; see [D44](DECISIONS.md).

## Flux compatibility surface

Everything Hecate depends on from Flux, in one place, so the blast radius of any Flux
change is knowable without reading the code.

**API versions read** (never written — Hecate does not create or mutate Flux resources):

| Kind | Group/version |
|---|---|
| `Kustomization` | `kustomize.toolkit.fluxcd.io/v1` |
| `HelmRelease` | `helm.toolkit.fluxcd.io/v2` |
| `GitRepository`, `OCIRepository`, `HelmChart`, `HelmRepository`, `Bucket` | `source.toolkit.fluxcd.io/v1` |

**Status fields read:**

```
metadata.generation            vs status.observedGeneration   (staleness)
status.conditions[Ready]       status / reason / message / lastTransitionTime
status.conditions[Stalled]     status / reason / message
status.lastAppliedRevision     Kustomization
status.lastAttemptedRevision   HelmRelease
status.artifact.revision       source kinds
status.history[0]              HelmRelease chartVersion / digest
spec.suspend
```

**Events emitted** (not read): standard Kubernetes Events, which Flux's
notification-controller picks up and dispatches. Hecate ships no notifier of its own.

**Cross-namespace references are refused by default**, matching Flux's own
`--no-cross-namespace-refs=true` posture across its controllers. See
[D11](DECISIONS.md).

**Why this section exists.** We read Flux as `unstructured`, which means an API version
removal cannot break our build — it breaks silently at runtime. Flux does remove API
versions. Two mitigations make that survivable: a startup discovery check that warns
when a served version differs from our default, and status fixtures captured per
supported Flux minor so a contract change fails a unit test in milliseconds. See
[D4](DECISIONS.md), amended.

## Upgrading the control plane

What the API promises, and what happens to work already under way.

**`v1alpha1` promises nothing, and that is what alpha means.** Any field may
change or be removed in the next alpha, per the lifecycle policy in the
development plan. A stable field list is a `v1` deliverable, not something to
assert early — declaring stability we have not earned is worse than declaring
none, because the second is checkable.

**CRDs go first, and Hecate enforces it.** Helm installs a chart's `crds/` once
and never touches them on upgrade, so a chart upgrade alone ships a new
controller against the old API — and the API server prunes unknown fields
silently rather than rejecting them. A controller would then write
`status.evidence` and read back nothing, for ever, with no error anywhere.

So the controller checks at startup and refuses to run against CRDs older than
itself, naming the fields that are missing:

```
the cluster's CRDs are older than this build of Hecate.
  gates.hecate.dev
    status.evidence
    status.evidence.risk
    ...
Helm does not upgrade CRDs. Apply them, then restart:
  kubectl apply --server-side -f <release>/crds.yaml
```

A missed upgrade is therefore a failed rollout — loud, immediate, and holding
the previous version — rather than a behaviour change nobody can see. Measured:
removing one field from the Gate CRD put the new pod into CrashLoopBackOff with
the message above, while the old replica kept reconciling throughout, and
applying the CRDs let the rollout complete.

**An in-flight crossing survives the upgrade.** A Passage is the record of what
happened, held in its own status, so a controller that restarts picks up where
the object says it is:

- **Finished steps are not re-run.** Their phase, attempt count and
  `finishedAt` are already written down. Measured across a rolling restart: a
  succeeded `http` step kept `attempts: 1` and its original timestamp.
- **The running step is re-entered, not restarted.** D5 requires steps to be
  re-entrant precisely so this is safe; the step's attempt count continues
  upward and its work resumes.
- **The scratch directory is lost**, and that is designed for (D19). A
  `git-clone` clones again. Making it durable would mean a PersistentVolume per
  Passage to avoid re-running a clone.
- **The crossing does not restart from the beginning**, and `status.startedAt`
  does not move — which matters, because it is what the lead-time metric is
  measured from.

**What an upgrade does not do** is change what an in-flight Passage is running.
`spec.steps` is copied from the Gate at creation — see the field's own
documentation in `api/v1alpha1/passage.go` — so a Gate edited during
the upgrade — or a step whose behaviour changed between versions — cannot
retroactively alter a crossing that is already under way. The crossing finishes
under the rules it started with.

## The step engine

Steps are **invoked repeatedly rather than blocking**. A step that is waiting returns
`Running` with a `retryAfter`; the engine persists progress to the Passage and calls
again later.

All progress therefore lives in the Passage object rather than in process memory, so a
controller restart resumes mid-Passage instead of starting over. Long waits — a Flux
reconciliation, a pull request review, a change-gate hold — cost no goroutines.

Two error classes:

- **ordinary** — retried, and forgiven by `continueOnError`
- **terminal** — bad configuration, a missing reference: anything that will fail
  identically next time. Stops the Passage even when `continueOnError` is set, because
  `continueOnError` is meant to forgive flakiness, not typos.

Outputs flow forward by alias (`as: commit` → `${{ steps.commit.sha }}`) and are
rebuilt from persisted status on resume, so an interrupted Passage sees exactly what
an uninterrupted one would.

## Repository layout

```
api/v1alpha1/         Beacon, Bundle, Gate, Passage. The whole API.
pkg/flux/             Flux status evaluation. Pure; no client, no I/O.
pkg/health/           Checker interface, registry, and the Flux checker.
pkg/passage/          Step interface, registry, and the execution engine.
pkg/passage/steps/    Built-in steps.
charts/hecate/crds/   Generated CRDs.
```

`pkg/flux` takes an object and returns a verdict. It performs no I/O, which is why its
tests need no cluster and run in milliseconds.

## Testing

Everything above is tested without a cluster: 33 tests, ~0.2s. Fake clients for the
Kubernetes boundary, scripted runners for the engine, table-driven fixtures for status
evaluation.

Anything that *can* be tested without a cluster **must** be. End-to-end tests against
kind exist to catch what fixtures cannot — real Flux status output — not to substitute
for unit tests.
