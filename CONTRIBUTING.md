# Contributing

Hecate is pre-alpha, which makes this an unusually good moment to argue with the
design. If you run Flux across more than one environment, an issue describing
how promotion actually works for you is worth more than a patch.

## Before proposing a structural change, read the decisions

[`docs/DECISIONS.md`](docs/DECISIONS.md) records every non-obvious call and,
more usefully, the options that were **considered and rejected** — no pipeline
object, no notifier, no monorepo decomposition in Beacon, no `ExternalArtifact`
output. If your idea is in there, the answer may already exist along with the
reasoning, and disagreeing with the reasoning is a much better conversation than
rediscovering it.

If a decision is wrong, say so. They are dated judgements, not scripture, and
several are explicitly "rejected until someone has the use case".

## Getting set up

Everything is pinned in `flake.nix` — Go, kubectl, Helm, k3d, the Flux CLI,
controller-gen — at the versions CI uses.

```bash
nix develop          # or: direnv allow

make test            # the cluster-free suite, ~1s
make check           # what CI enforces: gofmt, vet, tests, generated output
make lint            # golangci-lint, statix, deadnix
```

Full setup, including the dev cluster and agenix secrets:
[`docs/ONBOARDING.md`](docs/ONBOARDING.md).

## What is expected of a change

**Tests that can fail.** Anything testable without a cluster must be — that is
the bar, and it is why the suite runs in about a second. More importantly:
before you trust a new test, break the code it covers and watch it fail. Several
bugs in this repository were found that way, and at least one test in here
passed for the wrong reason until someone did it.

**`make generate` after touching `api/` or a kubebuilder marker.** CRDs and RBAC
are generated; CI fails if the committed output has drifted, because a stale CRD
silently prunes fields the code sets.

**Comments that say why, not what.** The code is read far more often than it is
written, usually by someone debugging at an unhelpful hour. A comment explaining
why a thing is done the surprising way is worth ten describing the obvious way.

**Honest claims.** If something is not implemented, the README, the docs and the
issue should say so. A statement that overstates what works is a bug with a
longer fuse than most.

## Kinds of change

**A new step** goes in `pkg/passage/steps/`, implements `passage.Runner`, and
should implement `passage.Checker` too so a Gate is refused at admission rather
than part-way through a crossing with a commit already pushed. Steps must be
re-entrant: the engine will call yours again after a requeue, and a controller
restart loses the work directory entirely ([D19](docs/DECISIONS.md)). Document
it in [`docs/STEPS.md`](docs/STEPS.md).

**Anything reading Flux status** belongs in `pkg/flux`, which is pure — no
client, no I/O — and is tested against
[status captured from a real Flux](pkg/flux/testdata/), never hand-written. If
you add a case, capture it; hand-written status only ever confirms the
assumption that produced it.

**UI changes** need a test in `ui/test/`. The fixtures there are copied from
real API responses rather than written by hand, because the types in
`lib/api.ts` are hand-written against the Go structs and have been wrong —
`Explanation` once had `reason` and `remedy` where the server sends `kind` and
`fix`, which compiled perfectly and rendered blanks. A fixture invented from the
same memory as the type cannot catch that.

**A new e2e test** must be claimed by a phase in `.github/workflows/e2e.yml`.
There is a step that fails the build otherwise, because a test that runs in
neither phase is a test that never runs, and that has happened.

## Pull requests

Small and focused. A commit message that explains the reasoning is more valuable
than a tidy diff — look at `git log` for the register: what changed, why, what
was rejected, and what is still not covered.

CI runs the unit suite, lint, a Nix build, and the e2e matrix across three Flux
minors and three Kubernetes versions. All of it must be green.

You keep the copyright to your contribution; by opening a pull request you agree
it is licensed under [Apache 2.0](LICENSE) like the rest of the project. There
is no CLA.

## Security

Do not open a public issue for anything exploitable — see
[SECURITY.md](SECURITY.md).

## Conduct

[Contributor Covenant](CODE_OF_CONDUCT.md), reported through GitHub.
