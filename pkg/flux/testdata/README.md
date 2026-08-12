# Captured Flux status

Real output from a real Flux, one directory per minor version. Nothing in here
is hand-written, and that is the point: hand-written status is status we already
believe in, so it can only confirm the assumption that produced it.

The first capture earned its keep immediately. Flux writes
`status.observedGeneration: -1` on a resource that has never successfully
reconciled, while the conditions on the same object already describe generation
1 correctly. Hecate believed the top-level field, so a GitRepository with a wrong
URL reported *Progressing* for ever — never Degraded however long it failed, with
"repository not found" hidden behind a message about generations. No
hand-written fixture would have found that, because we would have written the
sentinel we expected.

## What is here

`v2.7`, `v2.8` and `v2.9` — the three minors Flux itself supports, matching the
e2e matrix. Each holds the same nine files, captured from the **same** sources
so the three are comparable: a diff between versions shows a contract change and
nothing else.

| | healthy | failing |
|---|---|---|
| GitRepository | ✓ | ✓ |
| OCIRepository | | ✓ |
| HelmRepository | ✓ | ✓ |
| Kustomization | ✓ | ✓ |
| HelmRelease | ✓ | ✓ |

All three versions agree on every field Hecate reads, including the
`observedGeneration: -1` sentinel — and the workload kinds carry it too, so D45
is a rule about Flux rather than a quirk of GitRepository.

**Each kind reports its revision somewhere different**, and *where* is the
contract: a GitRepository under `status.artifact.revision`, a Kustomization
under `status.lastAppliedRevision`, and a HelmRelease under
`status.lastAttemptedRevision` — it has no `lastAppliedRevision` at all. A
release moving one of those is the change that would otherwise surface as a
`flux-wait` hanging to its deadline on a resource that converged ages ago.

## Adding a version

Bring up a cluster running the Flux minor you want, then create sources that
succeed and sources that fail, and capture what Flux says about them:

```bash
kubectl create ns fixtures
kubectl apply -f - <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: { name: git-broken, namespace: fixtures }
spec:
  interval: 1m
  url: http://example.invalid/nope.git      # a failure, on purpose
  ref: { branch: main }
YAML

# ...wait for it to reconcile, then, for each object:
kubectl get gitrepository git-broken -n fixtures -o json \
  | jq '.metadata |= {name, namespace, generation}' \
  > pkg/flux/testdata/v2.9/gitrepository-failing.json
```

Only `name`, `namespace` and `generation` are kept from the metadata —
`managedFields`, UIDs and resourceVersions are noise that would churn the diff
on every recapture. `spec` and `status` are kept verbatim; **do not tidy the
status**, since the awkward parts are the whole reason the file exists.

The tests discover version directories themselves, so a new one needs no code
change — and `TestEveryFluxVersionHasTheSameFixtures` fails if a version is
missing one, because a kind quietly losing coverage on one Flux while the suite
still passes is the failure this directory exists to prevent.

Nothing asserts a literal revision: the expected value is read back out of each
fixture, so a version captured from a different commit needs no test change.

## The failing fixtures matter more than the healthy ones

A source that works looks the same everywhere. A source that is broken is where
the contract shows: which condition carries the reason, whether
`observedGeneration` is a sentinel, whether an artifact from a previous success
is still reported. Those are the fields a Flux release can change under us, and
the ones a naive check gets wrong.

Use the same URLs each time. Capturing v2.7 from one repository and v2.9 from
another makes the two impossible to diff, which throws away most of the value.

## Bucket

Not captured — it needs an object store to point at, and the status contract is
the same as the other source kinds (`status.artifact.revision`, standard
conditions). The API version is mapped in `pkg/health`, so a Gate can watch one;
it simply has no fixture behind it. Worth adding if anyone runs one.
