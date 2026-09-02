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

## The six roles

Installed with `--set userRoles.create=true`, as ClusterRoles you bind where you
need them.

| Role | Grants | For |
|---|---|---|
| `hecate-viewer` | read the four kinds | anyone who needs to see what is happening |
| `hecate-promoter` | viewer, plus `create`/`update` on `passages` | asking a Gate to cross a Bundle, and aborting one |
| `hecate-approver` | viewer, plus `update` on `bundles/status` | signing off a Bundle for a Gate |
| `hecate-poker` | `update` on `beacons`, and nothing else | a CI job telling a Beacon to look now |
| `hecate-flux-operator` | `get`/`list`/`patch` on the Flux kinds a Gate can watch — Kustomizations, HelmReleases and the source kinds | suspending, resuming and reconciling what a Gate depends on. **A bigger right than promoting**: a suspension stops every future deploy of a resource and is state git will not restore, so it outlives whoever did it |
| `hecate-author` | viewer, plus `create` on `gates/author` | opening a pull request that proposes a Gate's step list (#172) — the write lands in a fleet repository, for a human to review |

The last two are deliberately unbound by default, and neither is implied by
`hecate-promoter`.

`gates/author` is a **virtual subresource**: no API object answers to it, so
the grant authorises Hecate's authoring endpoint and nothing else. It is not
`create gates`, which would be a live write to this cluster — a subject holding
that could `kubectl create -f gate.yaml` and skip the review the feature exists
for, and a hand-written Gate names the evidence server it trusts. See
[D66](DECISIONS.md).

Verified against a real API server rather than asserted — bind each role to a
service account and ask:

```console
$ kubectl auth can-i create passages.hecate.dev --as system:serviceaccount:ns:promoter
yes
$ kubectl auth can-i update bundles.hecate.dev --subresource=status --as system:serviceaccount:ns:promoter
no
$ kubectl auth can-i create gates.hecate.dev --subresource=author --as system:serviceaccount:ns:author
yes
$ kubectl auth can-i create gates.hecate.dev --as system:serviceaccount:ns:author
no
```

That last pair is the whole of D66: an author may ask Hecate to open a pull
request, and may not create a Gate.

| | read | promote | abort | approve | poll | operate flux | author |
|---|---|---|---|---|---|---|---|
| viewer | **yes** | no | no | no | no | no | no |
| promoter | **yes** | **yes** | **yes** | no | no | no | no |
| approver | **yes** | no | no | **yes** | no | no | no |
| poker | no | no | no | no | **yes** | no | no |
| flux-operator | no | no | no | no | no | **yes** | no |
| author | **yes** | no | no | no | no | no | **yes** |

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

`hecate-flux-operator` is separate because it is *larger*, not smaller. Its
rules are on Flux's own resources rather than Hecate's, because that is what is
actually written: someone who may already patch Kustomizations has this right
without Hecate's help, and someone who may not should not gain it by holding a
Hecate role. The check follows the same rule at request time — the API
authorises against the group and resource the Gate's watch actually resolves
to, not against a fixed `patch kustomizations`, so a Gate watching a
`HelmRelease` needs `patch helmreleases`. Hecate patches with its own
ServiceAccount, so this check is the only one that happens.

A cluster where every promoter could silently stop reconciliation is not a safe
default, so the role is unbound and the UI explains the refusal rather than
hiding the buttons — a control that vanishes teaches nobody that the permission
exists.

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

## Credentials to git hosts

Separate from the permissions above, which are Kubernetes'. These are what
Hecate presents to GitHub or GitLab, and they live in a Secret named by a
`credentialsRef`.

**Prefer a GitHub App.** A Secret carrying `clientID`, `installationID` and
`privateKey` mints an installation token that expires in an hour and is scoped
to the installation. A personal access token in a Secret is long-lived, broadly
scoped, and rotated by whoever remembers — for a tool whose pitch is that
promotion should be evidence-gated rather than merge-rights-gated, a permanent
write credential to every fleet repository is the weakest part of the threat
model.

```yaml
stringData:
  clientID: Iv1.abc123           # or appID
  installationID: "42"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
  baseURL: https://ghe.acme.example/api/v3   # Enterprise Server only
```

One Secret serves both halves of a promotion. The installation token works as a
git password as well as an API token, so the same credential pushes the commit
and opens the pull request — two credential paths, one short-lived and one not,
would leave the permanent one in place and change nothing.

A Secret with `token`, or `username` and `password`, still works and is
unchanged. The App path is an addition.

## The controller's own permissions

Separate from all of the above, in `charts/hecate/rbac/`. It is generated from
the kubebuilder markers on the controllers, so it is exactly what the code
reads and writes and cannot drift from it: `make generate` regenerates it and
CI fails if the committed output has changed.
