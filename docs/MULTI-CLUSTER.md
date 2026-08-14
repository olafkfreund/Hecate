# More than one cluster

The short version, because it decides everything else:

> **Promoting to another cluster needs no access to it.**
> Hecate needs credentials only to *check* a cluster, never to change one.

A promotion is a git write (D3). Hecate clones the fleet repository, repins an
image, commits and pushes. Flux in each cluster pulls that commit and applies
it. Nothing reaches out from Hecate to a workload cluster to deploy, so adding a
cluster to your pipeline does not mean giving Hecate the ability to change it.

That is worth stating plainly because the usual shape of a multi-cluster
deployment tool is the opposite: a central service holding admin credentials for
every cluster it deploys to, which is a single object worth compromising.

## What credentials are for

They are for **verification**, and verification is read-only.

A Gate can watch Flux resources in another cluster, and a Passage can wait until
Flux *there* has applied the commit it just pushed. Both need to read that
cluster's Flux objects. Neither writes anything, with one exception:
`flux-reconcile` patches an annotation to ask Flux to sync now rather than at
its next interval — a doorbell, not a desired state.

So the least privilege that works is:

| Step or check | Needs |
|---|---|
| Flux health check | `get`, `list`, `watch` on Flux kinds in the Gate's namespace |
| `flux-wait` | the same |
| `flux-reconcile` | the above plus `patch` on those kinds |

Nothing needs `create` or `delete` on anything, anywhere.

## Connecting one

A cluster is a Secret holding a kubeconfig under `value` — the same key Flux
uses for the same thing, so an operator who has already wired one for a
Kustomization does not have to learn a second convention.

```bash
kubectl -n team-a create secret generic staging-cluster \
  --from-file=value=/path/to/kubeconfig
kubectl -n team-a label secret staging-cluster hecate.dev/cluster=true
```

The label is what makes it findable. Without it the Secret still works, but
nothing can list it as a cluster — the settings screen would only see it once
some Gate happened to name it, and a kubeconfig you stored and cannot see is
indistinguishable from one that failed to save.

The settings screen does both steps for you, and probes the credentials on the
way in.

## Using one

`clusterRef` goes in the `with:` block of any of the three:

```yaml
watch:
  - uses: flux
    with:
      clusterRef: { name: staging-cluster }
      resources:
        - { kind: Kustomization, name: podinfo }

passage:
  steps:
    - uses: flux-reconcile
      with:
        clusterRef: { name: staging-cluster }
        resources:
          - { kind: GitRepository, name: fleet }
    - uses: flux-wait
      with:
        clusterRef: { name: staging-cluster }
        resources:
          - { kind: Kustomization, name: podinfo }
        expectedRevision: ${{ steps.commit.sha }}
```

The Secret is read from the Gate's own namespace, with Hecate's own rights. It
is not a per-user credential and does not become one.

## The tenant rule does not stop at the cluster boundary

**A Gate in `team-a` sees only `team-a` on the remote cluster**, exactly as it
does locally. `clusterRef` does not widen what a Gate may look at; it changes
which cluster the same narrow question is asked of.

This is deliberate and it is the part most likely to surprise. The alternative —
a cluster reference that also relaxes the namespace restriction — would make
`clusterRef` the way around multi-tenancy: add a kubeconfig, watch everything
everywhere. See D11, and `--no-cross-namespace-refs`, which Flux ships on its
own controllers for the same reason.

## Modelling environments across clusters

There is no cluster field on a Gate, and there does not need to be. A Gate
already names the path it writes and the Flux objects it waits on, and those
two together identify an environment:

```
fleet/
  clusters/
    eu-staging/podinfo/kustomization.yaml     <- staging Gate writes here
    eu-prod/podinfo/kustomization.yaml        <- production Gate writes here
```

Each cluster's Flux is pointed at its own path. The staging Gate writes
`eu-staging` and waits on the Kustomization in the staging cluster; production
writes `eu-prod` and waits on its own. Adding a region is a directory and a
Gate, not a new concept.

## When it goes wrong

**"clusterRef is set but this checker has no cluster resolver"** — the check ran
against a build wired without one. Every shipped build has it; a custom
embedding may not.

**"cannot reach the cluster in `<secret>`"** — reported as `Unknown` rather than
`Degraded`, on purpose. An unreachable cluster is a fact about the network, not
a verdict about the workload, and reporting it as unhealthy would have a Gate
refuse to promote because a *different* cluster was unreachable.

**Credentials that have expired** look exactly like credentials that work, right
up until a Passage is waiting on them. The settings screen probes each connected
cluster with a version read — the cheapest question an apiserver answers — so
this is visible before it is urgent rather than after.
