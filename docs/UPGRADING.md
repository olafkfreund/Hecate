# Upgrading Hecate

What is safe to depend on, and what happens to work already in flight when you
roll the control plane.

Hecate is `v0.x` and its API is `v1alpha1`. Both of those are honest labels
rather than modesty — see [What is stable](#what-is-stable) for what that
actually permits. This document is the contract until `v1.0`, and #54 is the
issue that closes when the field table below stops saying "proposed".

---

## Upgrading a release

### 1. The CRDs first, always

**`helm upgrade` does not update CRDs.** A chart's `crds/` directory is installed
once and never touched again. That is documented Helm behaviour rather than a
bug, and it is the single most common way to end up with a new controller
talking to an old API.

It fails quietly if you let it. The API server *prunes* fields it does not
recognise instead of rejecting them: the object stores without them, `kubectl`
reports success, and the controller reads a zero value. A `Gate` would go on
admitting Bundles while silently ignoring the field you just added.

So apply them yourself, before the chart:

```console
$ kubectl apply --server-side -f <release>/crds.yaml
$ helm upgrade hecate ...
```

`--server-side` matters on its own: CRDs outgrow the annotation that
`kubectl apply` uses to track client-side merges, and a large one fails with
`metadata.annotations: Too long`.

**You are not trusted to remember this.** The controller compares the cluster's
CRDs against the ones it was built with and refuses to start against an API
older than itself, naming the command that fixes it (#117). A rollout that fails
loudly and says why is worth more than a Beacon that has quietly stopped
honouring one of its fields. Hecate never writes CRDs — whoever owns the
cluster's CRDs keeps owning them.

### 2. Then the chart

```console
$ helm upgrade hecate oci://ghcr.io/olafkfreund/charts/hecate --version <x.y.z>
```

**CRDs newer than the controller are explicitly fine.** The startup check is
one-directional by design: it asks whether the cluster is *missing* anything the
binary needs, not whether the two match. Extra fields are simply unread. That is
what makes it safe to apply CRDs first and roll the controller second, and it
means rolling the controller back without touching the CRDs is a supported move
rather than a gamble.

Rolling the CRDs themselves *back* is not supported: the older schema prunes the
fields the newer objects were stored with, which is the silent data loss the
check exists to prevent, and by then it has already happened.

---

## Behaviour changes, by version

Changes that alter what Hecate *does* rather than what it offers — the kind that
look like a regression if you meet them without warning. New features are not
listed here; the release notes cover those.

### Unreleased

**`hecate approve` now refuses when Fides will not count the approval.** It
previously reported success.

Nothing has broken. The approvals in question never counted: Fides tallies an
approver as human only when the stored kind is `session`, and a bearer token
authenticates as a *service*. It honours the `on_behalf_of` delegation that
turns a service call into a human sign-off only when the server runs with
`FIDES_DELEGATED_APPROVAL_ENABLED=true`, the token holds the **Admin** role, and
the named identity is a registered user in the organisation. Miss any of those
and the request still returns `201`, the approval is stored, and the change gate
goes on holding — for a signature that has already been given.

If approvals start being refused after this upgrade, they were doing nothing
before it. Either configure delegation as above, or stop relying on Hecate to
record the sign-off and record it in Fides directly.

The same applies mid-crossing: an `evidence-gate` step that records a deployer
Fides will not count now fails the Passage terminally instead of retrying.
Retrying cannot help — no amount of waiting turns a service approval into a
session one — and the alternative was a crossing that waited for ever.

**This sits awkwardly with least privilege, and knowingly so.** Delegation
requires Admin, which is the opposite of the Writer-scoped account we recommend
and use for recording evidence. There is no good answer on Hecate's side; see
issue #132.

## What happens to in-flight promotions

**They resume. They do not restart, and they are not run twice.**

This is a design property rather than an accident, and it comes from D5: all
progress lives in the `Passage` object, so the only thing a controller keeps in
memory between reconciles is nothing at all. A step that is waiting returns
`Running` with a `retryAfter`; the engine persists what happened and calls again
later. Waits that last hours — a pull request review, a change-gate hold — cost
no goroutines and survive a redeploy.

Concretely, across a rollout:

| | what happens |
|---|---|
| A `Passage` mid-crossing | Resumes at the step it had reached. The step is told its attempt count, so it can tell a resumption from a first run. |
| The step's work directory | **Lost, and that is fine.** It is local scratch keyed by Passage UID. Steps are re-entrant by requirement — `git-clone` clones again. |
| A `Passage` that just finished | Left alone. A finished Passage is a permanent record, not work to redo. |
| A `Gate` waiting on health | Re-evaluated from the cluster's actual state; nothing is cached across the restart. |
| A `Bundle` | Immutable and content-addressed. Nothing to migrate. |

`pkg/passage/upgrade_test.go` demonstrates the first three by replacing the
Reconciler mid-crossing and deleting its work directory — sharing only the API
server, which is the only thing a rollout actually preserves. Before that test
these were design intentions with nothing showing they held.

Worth knowing if you go looking: **"a finished Passage is never re-run" is
guarded three times independently** — the engine returns early on a terminal
phase, skips steps that are already terminal, and the controller returns before
calling the engine at all. Breaking any one of them changes nothing observable.
That is deliberate; re-running a finished promotion means deploying something
nobody asked for.

### What is not covered

A promotion is not transactional, and an upgrade does not make it so. If the
controller is killed *between* a step's side effect and the status write that
records it, the step runs again on resume — which is exactly why steps must be
re-entrant, and why `git-commit` is written to be. A step you write yourself
that is not re-entrant will be run twice one day.

---

## What is stable

### The API version

`v1alpha1` is a Kubernetes convention with a specific meaning, and Hecate means
it: **fields may be renamed or removed, and there is no conversion webhook.** In
practice the shapes below have been stable for some time and are unlikely to
move, but "unlikely" is the commitment on offer until `v1.0`, not "will not".

What Hecate does commit to before then:

- **No silent field removal.** A field that goes away goes away in a release
  whose notes say so.
- **Stored objects keep working across a patch release.** `0.3.1` reads what
  `0.3.0` wrote.
- **The startup check, not your incident.** Any CRD change ships with a
  controller that refuses to run against the old one.

### Fields

> **Proposed, pending sign-off — this table is the half of #54 that is not yet
> settled.** It reflects what the code does today, not a decision anyone has
> made about what to promise. Do not cite it as a commitment yet.

| kind | fields |
|---|---|
| `Beacon` | `interval`, `watch`, `emit`, `retain`, `suspend` |
| `Gate` | `admits`, `passage`, `watch`, `verify`, `auto`, `windows`, `suspend`, `vars`, `retain`, `evidence` |
| `Passage` | `gate`, `bundle`, `steps`, `vars`, `actor`, `abort` |
| `Bundle` | `beacon`, `digest`, `alias`, `artifacts` |

`Bundle` is the one to be most careful with: its `digest` is content-addressed
and its name is derived from it, so a change to how a digest is computed renames
every Bundle and re-emits the lot.

### The Go API

`api/v1alpha1` is importable, and people do import it for the types alone.
`AddToScheme` and `GroupVersion` are the surface to depend on.

`SchemeBuilder` is exported but is not a stable shape — it changed type in the
release that dropped controller-runtime from this package, so `Register` now
takes functions rather than objects. Nothing needs to call it; use
`AddToScheme`.

### Step names and their config

Step `uses:` names and their `with:` fields are part of the contract in the same
sense the CRDs are — a Passage that names `git-commit` should keep working. See
[STEPS.md](STEPS.md).

### Events

Event `reason` and `type` are what alerts are written against and are treated as
stable. The `action` field is newer; see D54.

### Not part of any contract

Log lines and their wording — alert on events and metrics, never on a log
message. Anything under `pkg/` that is not `api/v1alpha1`: Hecate is a
controller that happens to be written in Go, not a library.

Metric and dashboard stability is not settled here either. See
[OBSERVABILITY.md](OBSERVABILITY.md) for what exists; treat it as `v0.x` too
until this document says otherwise.
