# Decision record

Short entries. Why, not what — the code says what.

---

## D1 — Hecate is an independent implementation

**2026-08-10 · accepted**

A prototype established that an existing Apache-2.0 promotion engine could be adapted
to Flux with a small adapter, inheriting a large amount of machinery for very little
code.

We chose not to. Hecate is its own product: its own API, vocabulary, engine and
implementation, with no third-party promotion engine in the dependency graph.

**Why.** An adapter makes the roadmap someone else's to set. Every feature we care
about most — evidence-gated crossings, OpenTelemetry throughout, a UI that matches the
rest of the platform — would have been bolted onto a model designed around different
assumptions, and every upstream release would be a merge risk. Owning the model is the
point; the engine is the cheap part.

**Cost, accepted knowingly.** We reimplement the step library, control plane, API
server, RBAC and provider integrations. That is a large body of work. It is smaller
than it looks: most of the value in that layer is *wiring*, not original algorithms —
`go-git`, `helm.sh/helm/v3`, `sigs.k8s.io/kustomize/api` and `go-containerregistry` do
the actual work, and we call them directly.

**What we kept.** `pkg/flux` was written during the prototype with no dependency on
anything external, precisely so it would survive this decision. It did, unchanged.

---

## D2 — Four resources, and no pipeline object

**2026-08-10 · accepted**

Beacon, Bundle, Gate, Passage. The pipeline graph is derived from what each Gate
admits rather than declared in a fifth resource.

**Why.** A pipeline object is a second source of truth. It drifts from the Gates it
describes, and then the picture in the UI is a lie. Deriving the graph means it cannot
drift.

**Deliberately deferred.** Reusable step bundles, multi-target fan-out from one Gate,
and a project/tenancy wrapper. Namespaces already provide tenancy; the other two are
speculative until someone asks. Adding a resource later is easy. Removing one is not.

---

## D3 — Git is the rendezvous; Hecate never talks to Flux

**2026-08-10 · accepted**

A Passage writes to git or OCI. Flux syncs. Hecate reads Flux resource status back.
There is no direct call in either direction.

**Why.** Flux stays authoritative. Hecate stays removable — uninstall it and working
manifests remain in git. And the integration boundary is a data format rather than an
API contract, so supporting another delivery engine is a new step plus a new checker,
not a redesign.

---

## D4 — Flux resources are read as unstructured

**2026-08-10 · accepted**

`pkg/flux` reads Kustomizations and HelmReleases as `unstructured` rather than
importing the Flux controller API modules.

**Why.** Importing them adds four module dependencies and pins us to one Flux minor
version. The part of the contract we depend on — `observedGeneration`,
`conditions[Ready|Stalled]`, `lastAppliedRevision`, `spec.suspend` — is stable across
v1 and v2.

**Revisit if** we need a field the documented status contract does not expose.

**Amended 2026-08-10 — the cost we failed to write down.** Trading compile-time coupling
for version tolerance means an API version change **cannot break our build, so it breaks
silently at runtime**. Flux does remove API versions: `image.toolkit.fluxcd.io/v1beta2`
and `notification.toolkit.fluxcd.io/v1beta2` reached end-of-life in Flux v2.9.

The decision stands, but it is only safe with two mitigations, both now required:

1. **Startup API discovery** — warn loudly if a configured kind's served version differs
   from the version we default to.
2. **Status fixtures per supported Flux minor** — `pkg/flux` tests read real captured
   status output for each Flux version we claim to support, so a contract change is a
   unit-test failure rather than a support ticket.

An unstructured reader without these is not version-tolerant, merely version-oblivious.

---

## D5 — Steps are polled, not blocked

**2026-08-10 · accepted**

A step that is waiting returns `Running` with a `retryAfter`. The engine persists
progress to the Passage and calls again.

**Why.** All progress lives in the Passage object, so a controller restart resumes
mid-Passage instead of starting over. Waits that last hours — a pull request review, a
change-gate hold — cost no goroutines and survive a redeploy.

**Consequence.** Steps must be re-entrant. A step is handed its own attempt count and
prior outputs so it can tell a first invocation from a resumption.

---

## D6 — `Ready=False` is Progressing until a deadline

**2026-08-10 · accepted**

A Flux resource reporting `Ready=False` is Progressing, not Degraded, until either
`Stalled=True` or the condition has been failing longer than `failAfter` (default 10
minutes).

**Why.** Flux retries most failures forever, so `Ready=False` never becomes terminal
on its own. Treating it as failure aborts Passages on transient faults; treating it as
Progressing forever reports a wedged deploy as "still working". A tunable deadline is
the only honest answer, and the right value is environment-specific — production
rollouts are legitimately slower than dev.

---

## D7 — Naming: Beacon, Bundle, Gate, Passage

**2026-08-10 · accepted**

**Why.** The alternative was plain-descriptive naming (Source, Release, Environment,
Promotion), which is instantly legible but heavily overloaded across the ecosystem —
"Source" in particular collides with Flux's own vocabulary, in a Flux-adjacent tool.

The chosen set is distinctive, searchable, unambiguous in context, and coherent with
the product name: Hecate *Kleidouchos* is the key-holder at the threshold. The cost is
one line of glossary, paid once.

---

## D8 — One operations layer, four consumers

**2026-08-10 · accepted**

The CLI, API server, MCP server and UI all call one `pkg/ops`. None of them
reimplements "what is eligible", "may this be approved", or "why is this stuck".

**Why.** Four consumers want the same handful of operations. Implemented separately
they diverge, and the divergence lands in the *rules* — which is the worst place to
have four answers. This is the actual early-architecture item; MCP and LLM support are
thin adapters over it.

**Consequence.** `pkg/ops` is built with the CLI, its first consumer, and nothing added
later may bypass it.

---

## D9 — One LLM client, not a provider abstraction

**2026-08-10 · accepted**

Ollama, llama.cpp, vLLM, LM Studio and the hosted vendors all expose an
OpenAI-compatible `/v1/chat/completions`. So "pluggable LLM" is a base URL, a model
name and an optional key — one ~100-line client, no interface, no registry, no factory.

**Why.** An interface with one meaningful implementation is a cost with no benefit. The
compatibility layer already exists at the wire level; reimplementing it as a Go
abstraction adds indirection and nothing else. If a provider genuinely does not fit,
*that* is the day to add a second implementation.

**Non-negotiable regardless of size.** Everything Hecate would feed an LLM — Flux
condition messages, commit messages, PR titles, image tags — is attacker-influenced.
Untrusted-data preamble and input caps ship in the first commit that sends a prompt.

**Hard limit.** An LLM never makes a promotion decision. Diagnosis is an assist;
Hecate is fully usable with none configured. Evidence gates decide what crosses.

---

## D10 — API group is `hecate.dev`

**2026-08-10 · accepted, with a dependency**

**Open action:** register `hecate.dev`. An API group on a domain we do not control is
a problem at ecosystem-listing or donation time, and the group is painful to change
once it is in users' manifests.

---

## D11 — Cross-namespace references are refused by default

**2026-08-10 · accepted**

The controller takes `--no-cross-namespace-refs`, defaulting to **true**. With it set, a
Gate's `watch[].namespace` naming anything other than the Gate's own namespace is
rejected at admission with an explanatory message.

**Why.** Flux's [security best practices](https://fluxcd.io/flux/security/best-practices/)
say it directly of tools like ours: *"Controllers integrating with Flux should adopt
identical patterns: cross-namespace restriction, service account defaults, workload
identity configuration, and network policy compliance."* Flux ships
`--no-cross-namespace-refs=true` across five of its own controllers.

Today `health.FluxResource.Namespace` accepts any namespace and merely *defaults* to the
Gate's. In a multi-tenant cluster that lets one team's Gate watch another team's
Kustomization — a tenant-isolation hole, and a channel for inferring what other teams
are running.

**Why now.** Default-open to default-closed is a breaking change the moment anyone's
manifests rely on it. Free today.

**Escape hatch.** Operators who genuinely run single-tenant can set the flag false. The
default is what matters; the flag exists so the default can be safe.

---

## D12 — Emit Kubernetes Events; never build a notifier

**2026-08-10 · accepted**

Hecate emits standard Kubernetes Events for Beacon discovery, Gate admission, Passage
lifecycle and health transitions. It ships no notification subsystem.

**Why.** Flux's [notification-controller](https://fluxcd.io/flux/components/notification/)
already watches Kubernetes Events and dispatches to Slack, Teams, Discord and webhooks.
Every Flux user has it and knows how to configure it. Emitting Events buys the entire
feature for the cost of a recorder, and users configure Hecate alerting exactly the way
they configure Flux alerting.

Building our own would mean a second place to configure notifications, a second set of
provider integrations to maintain, and a second thing to get wrong.

**Known limitation.** Flux's git commit-status providers need Kustomization-specific
metadata and will not work with Hecate events. Generic providers will. Accepted: commit
status is CI's job, not a promotion tool's.

---

## D13 — Bundles are garbage collected; status lists are capped

**2026-08-10 · accepted**

`Beacon.spec.retain` bounds how many unreferenced Bundles survive (default ~10).
`GateStatus.History` is capped at 10 entries.

**Why.** A Beacon on a two-minute interval emits Bundles indefinitely. Unbounded, that
is etcd growth with no ceiling and a `kubectl get bundles` nobody can read. `History`
is worse: an unbounded list inside a status object, rewritten on every reconcile.

**Safety rule.** GC never removes a Bundle that is currently in a Gate, referenced by a
Passage, or explicitly approved. Deleting the record of what is running in production
to save space would be a spectacular own goal for a tool whose product is the audit
trail.

**Long-term history** belongs in the evidence store (Fides), not in etcd. Kubernetes
objects are working state; the permanent record lives elsewhere.

---

## D14 — `ExternalArtifact` output: considered and rejected

**2026-08-10 · rejected**

Flux v2.9 ships
[ArtifactGenerator and ExternalArtifact](https://fluxcd.io/flux/components/source/artifactgenerators/)
via `source-watcher` — a sanctioned way for third-party tools to publish artifacts
directly into Flux. Hecate could produce `ExternalArtifact` instead of writing git.

**Rejected.** It would be faster and would need no repository write access, but it
removes the git round-trip — and the git round-trip *is* the audit trail. For a tool
whose differentiator is evidence-gated promotion, deleting the reviewable, revertible,
human-readable record of what changed and why trades away the product to save latency.

It would also break [D3](#d3--git-is-the-rendezvous-hecate-never-talks-to-flux):
publishing a Flux CR is talking to Flux.

**Revisit only if** a real user asks for it and explicitly accepts losing the git
history. Recorded here so it is not re-litigated every time someone reads the Flux
release notes.

**Related and *not* rejected:** consuming `ExternalArtifact` and
`ResourceSetInputProvider` as Beacon *inputs*. That is reuse, and it is planned.

---

## D15 — Beacon does not do monorepo decomposition

**2026-08-10 · accepted**

Beacon watches artifact sources. It does not grow path-pattern discovery, per-directory
artifact splitting, or content-based partial revisions for monorepos.

**Why.** Flux's ArtifactGenerator does exactly this, with path-pattern directory
discovery as of v2.9, and it does it at the source layer where it belongs. A competing
implementation in Beacon would be worse, would drift, and would put us in the position
of arguing with upstream about whose monorepo story is correct.

This is the maintenance strategy in miniature: **what the ecosystem absorbs, we
delete.**

---

## D16 — `cleared` means crossed *and* verified

**2026-08-11 · accepted**

A Gate records a Bundle in `BundleStatus.Cleared` only once the crossing succeeded
**and** that Gate's verification passed. A crossing that happened but failed
verification goes to `BundleStatus.Blocked` with the reason.

Downstream eligibility is therefore exactly `bundle.HasCleared(upstreamGate)` — no
extra field, no cross-Gate lookups.

**Why not read the upstream Gate's own status?** `GateStatus.History` is capped
([D13](#d13--bundles-are-garbage-collected-status-lists-are-capped)), so an older
Bundle's record ages out — and a Bundle that genuinely cleared staging would silently
become ineligible for production weeks later. The Bundle is the durable record; the
Gate's status is a view.

**Why not add `verified` to `GateCrossing`?** It would encode the same fact twice and
invite the two copies to disagree. `GateOccupant.Verified` already exists for the
Gate-side view.

**Consequence, and the one thing to get right when verification lands (#21):** today
`Cleared` is written on a successful Passage, which is correct for a Gate that declares
no verification. When verification is implemented, that single write is the place it
gates. Nothing else needs to change.

---

## D17 — Automatic crossings only move forward

**2026-08-11 · accepted**

Having crossed a Gate before does **not** make a Bundle ineligible; only being
*currently* in the Gate does. Automatic crossings pick the newest eligible Bundle, and
only when it is newer than the current occupant.

**Why.** The obvious rule — "ineligible if it has cleared this Gate" — makes rollback
impossible for ever, because the Bundle you want to go back to has by definition
cleared. But dropping that rule alone means two eligible Bundles cross each other
alternately without end, since neither is current once the other arrives. Ordering
solves both: older Bundles stay listed and reachable, and automation never reverses.

**Consequence.** Rolling back is deliberately a human action — create a Passage for the
older Bundle directly. A controller that can roll back on its own is a controller that
will, at the worst possible moment.

---

## D18 — Neither Bundles nor Passages carry owner references

**2026-08-11 · accepted**

Bundles are labelled `hecate.dev/beacon`, Passages `hecate.dev/gate`. Neither has an
owner reference to the object that created it.

**Why.** Owner references cascade-delete. Removing a Beacon would erase every Bundle it
emitted; removing a Gate would erase the record of every crossing through it. For a
tool whose product is the audit trail, deleting history as a side effect of deleting
configuration is the wrong default — and it is the kind of default nobody notices until
the audit.

**Consequence.** Cleanup is explicit, and belongs to the garbage collector under
[D13](#d13--bundles-are-garbage-collected-status-lists-are-capped)'s safety rule.
Controllers use label-based `Watches` rather than `Owns` to react to their children.

---

## D19 — Passage scratch space is local and disposable

**2026-08-11 · accepted**

Each Passage gets a directory keyed by its UID, created on first use and removed
when the Passage reaches a terminal phase. No PersistentVolume, no shared storage.

**Why UID and not name.** A recreated Passage must never inherit a previous one's
leftovers.

**What happens on a controller restart.** The directory is gone, and the Passage
resumes at the step it had reached — with an empty work dir. That is survivable
because [D5](#d5--steps-are-polled-not-blocked) already requires steps to be
re-entrant: `git-clone` clones again if the checkout is not there. Making scratch
durable would mean a PersistentVolume per Passage, which is a great deal of machinery
to avoid re-running a clone.

**The step contract this creates**, and the thing to get right in every step written
from here on: *a step must tolerate an empty work directory on any invocation, not just
the first.* A step that assumes an earlier step's output is still on disk will work in
testing and fail after a restart.

Revisit only if a step appears whose work genuinely cannot be redone — a very large
artifact download, say. The answer then is a cache keyed by content, not a durable
work dir.

---

## D20 — A Gate adopts the health checks its crossing emitted

**2026-08-11 · accepted**

A step may return health checks alongside its result. The Passage records them in
`status.watch`, and the Gate assesses them **in addition to** its own `spec.watch` —
but only from a Passage that *succeeded*.

**Why.** A `flux-wait` step already names the resources it waited for. Requiring the
operator to restate the same list in `gate.spec.watch` is duplication, and duplicated
configuration drifts: the day someone adds a HelmRelease to the step and forgets the
Gate is the day the Gate stops noticing it is broken.

**Why only on success.** A crossing that failed may have got no further than cloning a
repository. Adopting whatever it managed to emit would have the Gate reporting on
resources the crossing never reached.

**Why not have the step write `gate.spec.watch` directly.** Controllers do not write
user spec. The Passage records what it observed; the Gate decides what to do with it.

---

## D21 — Step failures carry a reason code, not just prose

**2026-08-11 · accepted**

A step failure is a `passage.StepError` with a stable, machine-readable `Reason`
in PascalCase — `GitAuthFailed`, `FluxDegraded`, `InvalidConfig` — recorded in
`StepStatus.Reason` alongside the human `Message`.

**Why now rather than later.** Eleven steps land in M2. Decided first, each is
written once; decided after, it is eleven retrofits — and every step written in
between bakes in prose-only errors. The cost here is timing, not code.

**Why a reason at all.** `Message` is right for a human reading one failure. It
is useless for anything reasoning across many: `hecate diagnose`, a dashboard
counting failure classes, an operator asking "is this the same problem as
yesterday?", an LLM summarising a stuck Passage. Those need to distinguish "the
git host rejected our credentials" from "Flux gave up" without parsing English.

**Not a closed enum.** Each step names its own failures. A central registry
would be a bottleneck and a permanent merge conflict, and steps live in
different packages. The convention — PascalCase, stable, Kubernetes-condition
style — is the whole contract. *Stable* is the operative word: a reason is
matched on, so renaming one breaks whatever was matching.

**Structured detail rides in the step's `Output`**, which already exists and
which the engine already records on failure. A second place to put facts would
only invite the two to disagree.

**Terminality is now a field, not a second type.** `Terminal` used to be its own
error type; it is a property of a failure, not a different kind of failure, and
two ways to express one thing is exactly the duplication that drifts. `Terminalf`
and `IsTerminal` are unchanged for callers.

**Not in metrics.** Reason is unbounded by design, and unbounded label values are
how a metrics backend falls over. It is for diagnosis, not aggregation.

---

## D22 — Expressions are evaluated by the engine, not by steps

**2026-08-11 · accepted**

`${{ ... }}` is interpolated into a step's `with:` block by the engine, before the
step runs. `step.if` is evaluated the same way. No step opts in, and no step can
forget.

**Why not per step.** Eleven steps land in M2. Interpolation done in each is
eleven chances to skip it, and the twelfth step someone adds later would be the
one that silently does not support variables.

**Whole-string expressions keep their type.** `${{ vars.replicas }}` yields the
number; `"v${{ vars.major }}"` yields a string. Configuration is JSON, so
stringifying everything would break every numeric and boolean field.

**expr-lang, not Go templates.** These expressions are evaluated over data an
attacker can influence — image tags, commit messages, pull request titles. expr
is sandboxed with no I/O and no arbitrary code; a template engine over cluster
data is an escalation path. The environment exposes only the Bundle, prior step
outputs, the Gate's vars, and a few strings about the crossing.

**Artifacts are looked up by repository, not by index.** `bundle.image('ghcr.io/acme/api')`
says what it means; `artifacts[0]` would silently pick the wrong one the day
somebody reorders a Beacon's watch list. A lookup that finds nothing returns nil
and the expression fails, rather than interpolating an empty string — promoting
`image:` with no tag is precisely the failure worth being loud about.

**A non-boolean `if` is an error**, not a truthiness guess. `if: "1"` is a
mistake, and treating it as true runs a step the author meant to gate.

**Vars are copied into the Passage** at creation, like steps, for the same
reason: editing a Gate must not change what an in-flight crossing sees.

## D23 — Commits are timestamped from the Passage, not from the clock

**Decision:** `git-commit` builds its author and committer signature from
`passage.status.startedAt`, not `time.Now()`.

D19 says a step must tolerate being called again with an empty or half-populated
work dir, so `git-commit` will re-run. If the commit carried wall-clock time, the
second run would produce a different SHA for identical content — a second commit
on the branch saying exactly what the first one said, and a `flux-wait` in the
same Passage waiting on a revision that no longer exists.

Deriving the timestamp from the Passage makes the commit a function of its
inputs: same tree, same message, same Passage, same hash. Re-running a crossing
is then a no-op rather than an accumulation, which is what makes retries safe.

**A clean tree succeeds** for the same reason: the second run finds the desired
state already committed, and reports HEAD so later steps still have a revision to
wait on. Failing there would make every retry unrecoverable.

**Pushing an already-pushed commit succeeds too.** go-git's
`NoErrAlreadyUpToDate` is the expected outcome of a re-run, not an error.
