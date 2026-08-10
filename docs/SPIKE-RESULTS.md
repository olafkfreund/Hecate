# Spike: can Hecate be a Kargo distribution with a Flux adapter?

**Date:** 2026-08-10 · **Kargo version tested:** v1.11.1 · **Verdict: yes, with three
fixable blockers.**

## The question

Kargo is a mature (~193k LOC non-test Go) promotion engine whose only real coupling to
Argo CD is two promotion steps, one health checker, and Argo Rollouts-based verification.
It is Apache-2.0. If its plugin points are usable from outside the repo, Hecate can supply
a Flux adapter and inherit the pipeline model, 34 engine-neutral promotion steps, the API
server, RBAC, SSO, sharding and GC — instead of reimplementing them.

So: **are `health.RegisterChecker` and `promotion.DefaultStepRunnerRegistry` usable by an
external Go module, and is the resulting adapter small?**

## Result

Yes. The adapter is **~370 lines of Go** (`pkg/flux` + `pkg/kargo`), builds clean against
`github.com/akuity/kargo v1.11.1`, and passes 10 test functions with no cluster and no
envtest.

```
ok  github.com/olafkfreund/hecate/pkg/flux    # 4 tests — pure, zero Kargo dependency
ok  github.com/olafkfreund/hecate/pkg/kargo   # 6 tests — adapter, fake controller-runtime client
```

The two plugin points work exactly as documented:

```go
promotion.DefaultStepRunnerRegistry.MustRegister(
    promotion.StepRunnerRegistration{Name: "flux-wait", Value: newFluxWaiter},
)
health.RegisterChecker(NewChecker(cl))
```

And `StepResult.HealthCheck` is the clean seam between them: `flux-wait` returns the
health criteria it just satisfied, so the Stage's continuous health check watches exactly
what the promotion waited for. No new protocol needed.

## Blockers found

### 1. Kargo is not consumable as an external Go module (workaround in place)

`github.com/akuity/kargo`'s own `go.mod` contains:

```
replace (
	github.com/akuity/kargo/api => ./api
	github.com/akuity/kargo/pkg/x/client/generated => ./pkg/x/client/generated
)
```

`replace` directives in a dependency are ignored by consumers, and neither submodule has
a published tag (`git ls-remote --tags` finds zero `refs/tags/api/*`). A plain
`go get github.com/akuity/kargo@v1.11.1` therefore fails to resolve
`github.com/akuity/kargo/api@v0.0.0`.

**Workaround (applied):** reproduce the replaces with pseudo-versions in *our* go.mod.

```
replace github.com/akuity/kargo/api => github.com/akuity/kargo/api v0.0.0-20260810153343-d2b5b9ff3ac6
replace github.com/akuity/kargo/pkg/x/client/generated => github.com/akuity/kargo/pkg/x/client/generated v0.0.0-20260810153343-d2b5b9ff3ac6
```

This works but pins us to a commit rather than a release, so Kargo upgrades are manual.

**Upstream fix:** ask Akuity to publish `api/vX.Y.Z` tags alongside each release. Small,
uncontroversial, benefits every downstream consumer.

### 2. `StepRunnerCapabilities` has no generic delivery-engine client

```go
type StepRunnerCapabilities struct {
	KargoClient     client.Client
	ArgoCDClient    client.Client   // <- Argo-specific by name
	CredsDB         credentials.Database
	GitUserResolver GitUserResolver
}
```

`ArgoCDClient` is semantically "the client for the cluster where the delivery engine's
resources live" but is named for one engine. The spike uses `KargoClient`, which assumes
Flux runs in the same cluster as the Hecate controller — the common case, but it rules
out remote Flux clusters.

**Upstream fix:** add a generic client (or rename to `DeliveryClient` with an alias).
Until then, remote-cluster Flux is unsupported.

### 3. `cmd/controlplane` is `package main`, so the binary cannot be composed by importing

Kargo's entry point is not importable. Hecate must either vendor those ~10 thin
cobra-wiring files or land an upstream PR moving them to `pkg/controlplane` with an
exported `Execute(ctx)`.

**Decision:** vendor for now (it is small and stable), PR upstream in parallel.

## Design notes worth keeping

**The core is deliberately Kargo-free.** `pkg/flux` reads Flux resources as
`unstructured` and evaluates the documented status contract. It imports only
`apimachinery`. If Path A ever fails, this package moves to a standalone controller
unchanged. That is why it is a separate package rather than inlined in the adapter.

**Unstructured over the typed Flux APIs.** Importing `kustomize-controller/api` and
`helm-controller/api` would add four module dependencies and pin us to one Flux minor
version. The status contract we rely on — `observedGeneration`, `conditions[Ready|Stalled]`,
`lastAppliedRevision`, `spec.suspend` — is stable across v1/v2.

**Three correctness details that a naive checker gets wrong**, all covered by tests:

- **Stale status is the false-green.** `Ready=True` with
  `observedGeneration < metadata.generation` describes the *previous* generation. Checked
  before conditions, and reported Progressing.
- **`Ready=False` is not failure.** Flux retries most failures forever, so a bare
  `Ready=False` never becomes terminal on its own. We report Progressing until either
  `Stalled=True` (Flux has given up) or the condition has been failing longer than
  `failAfter` (default 10m, tunable per step). Without that deadline a wedged deploy
  reports "still working" indefinitely.
- **Suspended is not Progressing.** `spec.suspend: true` means Flux will never reconcile;
  reporting Progressing would hang a promotion forever waiting on a human. Reported
  Unknown with an explicit issue.

## Recommendation

Proceed with Path A. Open the two upstream PRs (module tags, generic client) immediately,
since lead time on those is outside our control. Vendor `cmd/controlplane` to unblock the
first working binary.

**Scope correction discovered during the spike:** Kargo's UI is React + Ant Design; the
Fides UI is Next.js 16 + React 19 + Tailwind v4 + lucide-react + recharts. "Same UI as
Fides" therefore means Hecate's UI is a **genuine build, not a Kargo fork**. Path A gives
us the backend for free; it does not give us the frontend. This is the single largest
line item in the development plan.
