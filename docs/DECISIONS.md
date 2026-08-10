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
