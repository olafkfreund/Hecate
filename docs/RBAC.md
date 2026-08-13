# Permissions

Hecate does not implement an authorization system. Every permission is a
Kubernetes one, and every decision is made by the API server — the CLI talks to
it directly, and the API server behind the UI answers each request by asking
Kubernetes with a `SubjectAccessReview` before doing anything.

That is the whole design, and it has one consequence worth stating plainly: a
person with direct cluster access and the rights below can do these things
without going through Hecate at all. The CRD *is* the API. Hecate adds
eligibility rules on top — a Passage that the rules would refuse is still a
Passage somebody with `create passages` can write by hand — so the rules are a
workflow, and RBAC is the security boundary.

## The four roles

Installed with `--set userRoles.create=true`, as ClusterRoles you bind where you
need them.

| Role | Grants | For |
|---|---|---|
| `hecate-viewer` | read the four kinds | anyone who needs to see what is happening |
| `hecate-promoter` | viewer, plus `create`/`update` on `passages` | asking a Gate to cross a Bundle, and aborting one |
| `hecate-approver` | viewer, plus `update` on `bundles/status` | signing off a Bundle for a Gate |
| `hecate-poker` | `update` on `beacons`, and nothing else | a CI job telling a Beacon to look now |

Verified against a real API server rather than asserted — bind each role to a
service account and ask:

```console
$ kubectl auth can-i create passages.hecate.dev --as system:serviceaccount:ns:promoter
yes
$ kubectl auth can-i update bundles.hecate.dev --subresource=status --as system:serviceaccount:ns:promoter
no
```

| | promote | approve | poll | read | abort |
|---|---|---|---|---|---|
| viewer | no | no | no | **yes** | no |
| promoter | **yes** | no | no | **yes** | **yes** |
| approver | no | **yes** | no | **yes** | no |
| poker | no | no | **yes** | no | no |

Note the query: `--subresource=status` is required. `kubectl auth can-i update
bundles.hecate.dev/status` answers `no` for everyone, including a subject that
holds the right — a false negative that will convince you four-eyes is broken
when it is not.

## Why promote and approve are separate

An approval a promoter can grant themselves is not an approval. They are
different verbs on different resources — `create passages` against
`update bundles/status` — so a Role can carry one without the other, and
**nothing in Hecate has to enforce that**, because the API server already does.

This is the same reason the API server's own actions map to Kubernetes verbs
rather than to an internal permission model: there is one place to look when
asking who may do what, and it is the place your existing tooling already
audits.

`hecate-poker` is separate for the same kind of reason, at a smaller scale. A
webhook credential lives in CI, which is a place credentials leak from; one that
could also read every Gate in the namespace is worth stealing. It grants a
single verb on a single resource and carries no read rules at all, because the
poll endpoint answers with its own request token and needs no listing to work.

## How this relates to Fides segregation of duties

They answer different questions and neither replaces the other.

- **Kubernetes RBAC** answers *may this identity take this action?* It is
  checked before anything happens, and refusing is a 403.
- **Fides segregation of duties** answers *do the identities on this change
  satisfy four-eyes?* It is evaluated over evidence after the fact — committer,
  approver and deployer must be three distinct people — and refusing is a held
  change gate.

A person can hold `hecate-approver` and still fail segregation of duties,
because they also wrote the commit. That is not a contradiction: RBAC says they
are *allowed* to approve, and Fides says this particular approval does not make
the change compliant.

Hecate does not reimplement the distinctness check (D48). Fides evaluates it
over the identities recorded on the trail, and a second, weaker copy here would
drift from the one that matters. What Hecate is responsible for is making sure
the identities arrive: an approval is recorded `on_behalf_of` the human who gave
it rather than under Hecate's service token, or every approval would carry one
identity and four-eyes would evaluate one person having done everything.

An automatic crossing records no deployer at all. Hecate is not a person, and
naming it would let a change pass four-eyes with two humans and a robot — the
change gate then holds on "no deployer recorded", which is correct.

## The controller's own permissions

Separate from all of the above, in `charts/hecate/rbac/`. It is generated from
the kubebuilder markers on the controllers, so it is exactly what the code
reads and writes and cannot drift from it: `make generate` regenerates it and
CI fails if the committed output has changed.
