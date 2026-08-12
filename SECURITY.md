# Security policy

## Reporting a vulnerability

**Use [private vulnerability reporting](https://github.com/olafkfreund/Hecate/security/advisories/new).**
It is enabled on this repository, so the report stays between you and the
maintainers until there is a fix. Please do not open a public issue for
something exploitable.

What helps, roughly in order:

- what an attacker gets, and what they need to start with;
- the versions and configuration you saw it on — Hecate, Flux, Kubernetes;
- anything that reproduces it, even loosely.

You will get an acknowledgement within **3 working days**. If you do not, the
report has gone astray rather than been ignored — say so publicly without
details and we will find it.

## What happens next

| | |
|---|---|
| Acknowledged | within 3 working days |
| Assessment and severity | within 10 working days |
| Fix, or a plan with a date | within 30 days for high and critical |
| Public disclosure | on release of the fix, or **90 days** after the report, whichever is sooner |

The 90 days is a ceiling, not a target: it exists so a report cannot be sat on,
and we would rather ship in a week. If a fix genuinely needs longer we will say
so before the deadline rather than after it, and agree an extension with you.

Credit goes to the reporter in the advisory unless you ask otherwise.

## Supported versions

**There are no releases yet.** Hecate is pre-alpha and the only thing to run is
`main`, so today the answer is "fixed on `main`". That is not a support policy
and is not pretending to be one.

Once there are releases, security fixes go to the
[supported minors](README.md#compatibility) — the latest two — and the
[maintenance policy](docs/DEVELOPMENT-PLAN.md#7-maintenance-policy) is the fuller
statement.

## What is in scope

Hecate is a Kubernetes controller that writes to git repositories and reads Flux
resources. The things worth attacking are, in rough order of how much we care:

- **Escaping the Gate.** Anything that lets a Bundle enter an environment
  without satisfying the Gate's conditions — skipping approval, bypassing an
  evidence gate, forging an admission. The whole product is a claim about what
  may pass; a hole here is the worst kind.
- **Credential exposure.** Git tokens, registry credentials and Fides tokens
  reach Hecate through Secrets. Anything that puts one in a log, an event, an
  object's status, a commit message or a trace is a vulnerability, not a
  cosmetic issue.
- **Cross-tenant reads or writes.** Cross-namespace references are refused by
  default ([D11](docs/DECISIONS.md)); a way around that is in scope.
- **Executing what a Passage should not.** A crafted `with:` block, expression
  or artifact reference that runs something outside the step's intent.
- **Falsifying the audit trail.** A Passage's evidence, attestations or commit
  trailers claiming something that did not happen.

## What is not

- Vulnerabilities in Flux, Kubernetes, or a dependency — report those upstream;
  we will pick up the fix. If Hecate's *use* of a dependency makes an upstream
  issue exploitable when it otherwise would not be, that is ours.
- A cluster-admin doing something destructive. Hecate does not defend against
  the people who install it.
- Anything requiring write access to the fleet repository. If you can push to
  the branch Flux syncs, you do not need Hecate to deploy something.
- Missing hardening that is a configuration choice — running with
  `--no-cross-namespace-refs=false`, for example. Tell us if the *default* is
  wrong; that is a real report.

## What is in place today

- Private vulnerability reporting, which is why this document can point at it.
- Secret scanning and push protection on this repository.
- Weekly dependency updates, security updates immediately
  ([renovate.json](renovate.json)).
- Cross-namespace references refused by default, matching Flux's own posture.

## What is committed but not yet built

Stated separately because a security document that blurs the two is worse than
one that admits the gap:

- **Signed releases and SBOMs** (cosign, #55) — there are no releases yet, so
  there is nothing signed yet either.
- **Hecate's own release provenance recorded in Fides** (#57). If we will not
  gate our own releases with it we should not ask anyone else to, but today we
  do not.
- **Short-lived provider credentials** (#118). Git host access is a static token
  in a Secret; installation tokens that expire in an hour would be materially
  better and are not implemented.
