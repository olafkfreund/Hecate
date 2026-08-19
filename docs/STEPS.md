# Hecate — Step reference

A Gate's `passage.steps` is an ordered list. Each entry names a step with `uses`,
configures it with `with`, and optionally publishes its output under `as`:

```yaml
steps:
  - uses: git-commit
    as: commit                    # later steps read ${{ steps.commit.sha }}
    with: { message: "promote podinfo" }
    if: steps.edit.changed        # optional, must evaluate to a boolean
    continueOnError: false        # optional
```

Every `with:` value is interpolated before the step runs: `${{ ... }}` expressions
see the Bundle, the Gate's `vars`, and earlier steps' outputs. A whole-string
expression keeps its type, so `${{ vars.replicas }}` stays a number
([D22](DECISIONS.md)).

A Gate's steps are checked as soon as it is applied: an unknown step, a
duplicate `as` alias, a misspelt field or a missing required one marks the Gate
`Ready=False` with `InvalidSteps` and stops it opening any Passage
([D31](DECISIONS.md)). `kubectl describe gate` names the step index and the
field.

Two rules apply to every step:

- **Re-running one must be safe.** Scratch space is disposable and a controller
  restart re-runs from where the status left off ([D19](DECISIONS.md)), so steps
  are written to converge rather than accumulate — an already-correct file, an
  already-made commit and an already-open pull request are all successes.
- **Failures carry a reason code**, listed per step below, so a failure can be
  classified without reading English ([D21](DECISIONS.md)).

Paths are relative to the Passage's work directory. `git-clone` puts its checkout
in `repo/` by default, so a file in it is `repo/apps/production/values.yaml`.

---

## git-clone

Checks out a repository into the work directory.

| field | type | default |
|---|---|---|
| `repo` | string | required |
| `branch` | string | the remote's default |
| `path` | string | `repo` |
| `depth` | int | full history |
| `credentialsRef` | `{name}` | none (public, or ambient) |

An existing checkout of the same repository is reused rather than re-cloned, so
an edit step's uncommitted work survives a requeue. A checkout of a *different*
repository is replaced — committing to the wrong repository is the failure that
would otherwise be silent.

**Credentials.** The Secret may hold `identity` (+ optional `known_hosts`,
`password` for an encrypted key) for SSH, or `username` and `password` for HTTPS.
An SSH key wins when both are present. `known_hosts` is verified when supplied.

**Output:** `sha`, `branch`, `reused`.

**Reasons:** `GitAuthFailed`, `GitUnreachable`, `GitFailed`, `InvalidConfig`.

## Running a step after a failure

A step's `if:` is a bare expression, evaluated before the step runs. Two
variables exist for the outcome of the crossing so far:

| | |
|---|---|
| `failed` | an earlier step has already failed the Passage |
| `always` | true — the word for "run this whatever happened" |

```yaml
    steps:
      - uses: git-clone
        with: { repo: https://github.com/acme/fleet.git }
      - uses: set-image
        with: { path: repo/apps/production, image: ghcr.io/acme/podinfo }
      - uses: git-commit
        with: { message: "promote podinfo" }
      - uses: git-push
      - uses: flux-wait
        with:
          resources: [{ kind: Kustomization, name: podinfo, namespace: flux-system }]

      - uses: http                    # runs whether or not the crossing worked
        if: always
        with:
          url: https://example.com/hooks/deploys
          method: POST
```

Once a step has failed the Passage, the remaining steps **without** an `if:` are
recorded as `Skipped` — the happy path is over. Steps with an `if:` are still
evaluated, so `if: failed` and `if: always` both get their turn. That is what
makes it possible for a crossing to report its own outcome; without it, anything
reporting a result could only ever report success.

Two rules worth knowing:

- **A step that runs after a failure cannot wait.** The Passage is terminal at
  the end of that reconcile, so there is no later one to resume in. A step that
  returns `Running` there is recorded as `Failed` saying so.
- **The first failure is the one the Passage reports.** If the reporting step
  fails too — an expired token, say — its error is recorded against that step,
  but `status.message` still names the failure that actually broke the crossing.

See [D46](DECISIONS.md).

## git-commit

Stages everything in the checkout and commits it.

| field | type | default |
|---|---|---|
| `message` | string | required |
| `path` | string | `repo` |
| `paths` | \[string] | everything changed |
| `author` | `{name, email}` | `Hecate <hecate@users.noreply.github.com>` |
| `allowEmpty` | bool | `false` |

The signature's timestamp comes from `passage.status.startedAt`, not the clock,
so the same tree and message always produce the same hash and a re-run does not
stack a second commit ([D23](DECISIONS.md)). A clean tree succeeds and reports
HEAD, because that is exactly what a re-run looks like — with `committed: false`,
so a following step can tell the difference.

When tracing is configured, the commit carries the crossing's W3C trace context
as a trailer, so one trace can span the CI run, the promotion and the Flux
reconciliation ([D42](DECISIONS.md)):

```
promote podinfo to production

traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-a3ce929d0e0e4736-01
```

The trace ID is allocated once per Passage and persisted, so a retry writes the
identical trailer and therefore the identical commit. With no collector
configured there is no trace and no trailer.

**Output:** `sha`, `committed`.

**Reasons:** `WorkDirLost`, `GitFailed`, `InvalidConfig`.

## commit-status

Reports the crossing's outcome against the commit Flux applied, as a check on
the git host.

| field | type | default |
|---|---|---|
| `sha` | string | HEAD of the checkout |
| `path` | string | `repo` |
| `repo` | string | the checkout's origin remote |
| `provider` | `github` \| `gitlab` | inferred from the host |
| `baseURL` | string | the public host's API |
| `context` | string | `hecate/<gate>` |
| `state` | `Pending` \| `Success` \| `Failure` | what actually happened |
| `description` | string | "&lt;bundle&gt; crossed &lt;gate&gt;" |
| `targetURL` | string | none |
| `credentialsRef` | `{name}` | required |

**Run it with `if: always`.** On the happy path it can only ever report success,
and a check that is green whenever it appears looks like coverage and is not.
The engine tells the step whether the Passage failed ([D46](DECISIONS.md)), so
leaving `state` unset reports the real outcome rather than something the author
has to keep in step with the rest of the Passage.

```yaml
      - uses: flux-wait
        with:
          resources: [{ kind: Kustomization, name: podinfo, namespace: flux-system }]
      - uses: commit-status
        if: always
        with:
          credentialsRef: { name: forge }
```

`context` defaults to `hecate/<gate>` because the host de-duplicates on it: two
Gates reporting on the same commit need different names or the second silently
replaces the first.

**What it can and cannot say.** The commit is the one *Hecate* made and Flux
applied. It is not the application commit that produced the image — Hecate
resolves images to digests and never learns which source commit built one, so a
check claiming "this change reached production" would have a true-looking name
and no basis.

Two consequences of running after a failure, both from [D46](DECISIONS.md): the
step gets **one invocation** and cannot wait, which a single API call satisfies;
and if the status call itself fails, that error is recorded against the step but
`status.message` keeps the failure that actually broke the crossing.

`credentialsRef` needs a Secret with `token` (or `password`) — an API token,
which is not the same thing as push access. It also accepts a GitHub App
(`clientID`, `installationID`, `privateKey`), which is the better credential and
is preferred when the Secret carries both; see
[Credentials to git hosts](RBAC.md#credentials-to-git-hosts).

**Reasons:** `ProviderAuthFailed`, `ProviderFailed`, `WorkDirLost`,
`InvalidConfig`.

## git-push

Publishes the local commits.

| field | type | default |
|---|---|---|
| `path` | string | `repo` |
| `branch` | string | the checked-out branch |
| `toNewBranch` | bool | `false` — pushes to `hecate/<passage>` |
| `force` | bool | `false` |
| `credentialsRef` | `{name}` | none |

Pushing an already-pushed commit succeeds; it is the expected outcome of a
re-run. A rejected push stays retryable, because the branch may simply have
moved.

**Output:** `branch`, `sha`, `pushed`.

**Reasons:** `GitAuthFailed` (terminal), `GitUnreachable`, `GitRejected`,
`WorkDirLost`, `GitFailed`.

## git-pull-request

Opens a pull request — a merge request on GitLab — and by default waits for it
to merge.

| field | type | default |
|---|---|---|
| `credentialsRef` | `{name}` | required |
| `repo` | string | the checkout's origin remote |
| `provider` | `github` \| `gitlab` | inferred, public hosts only |
| `baseURL` | string | the public host's API |
| `head` | string | `hecate/<passage>` |
| `base` | string | the checked-out branch |
| `title` | string | `Promote <bundle> to <gate>` |
| `body`, `labels` | string, \[string] | — |
| `waitForMerge` | bool | `true` |
| `pollInterval` | duration | `1m` |

Waiting reports `Running` rather than blocking, so a review that takes days costs
nothing and survives a controller restart. The step publishes the **merge**
commit, not the branch's: a squashing host lands the change under a hash that
never existed locally, and that is what a later `flux-wait` must wait for. A
pull request closed unmerged fails terminally.

Opening is idempotent, and adopts one the host reports as already existing.

**Credentials:** a GitHub App (`clientID`, `installationID`, `privateKey`) —
preferred, and preferred over a token in the same Secret — or an API token under
`token`, or `password`. See
[Credentials to git hosts](RBAC.md#credentials-to-git-hosts).

**Self-managed hosts** must set both `provider` and `baseURL` — an appliance's
hostname says nothing about its flavour, and guessing wrong sends GitLab requests
to a GitHub API ([D25](DECISIONS.md)).

**Output:** `number`, `url`, `state`, `branch`, and `sha` once merged.

**Reasons:** `ProviderAuthFailed`, `ProviderFailed`, `PullRequestClosed`,
`InvalidConfig`.

## set-image

Repins an image in a kustomization's `images:` list.

| field | type | default |
|---|---|---|
| `path` | string | required |
| `image` | string | required — matched against an entry's `name` |
| `tag` / `digest` | string | taken from the Bundle |

The entry is found by the repository it names, not by position:
`images[2].newTag` repins the wrong image the day somebody reorders the list.
Whichever of `newTag` or `digest` the entry already uses is the field written —
switching between them changes the file's shape, and that is a decision to make
once by hand rather than on every crossing ([D24](DECISIONS.md)).

**Output:** `changed`, `file`, `image`, `pinned`.

**Reasons:** `KeyNotFound`, `EditFailed`, `InvalidConfig`.

## edit-yaml

Sets fields in a YAML file without reformatting it.

```yaml
- uses: edit-yaml
  with:
    path: repo/apps/production/values.yaml
    edits:
      - key: image.tag
        value: ${{ bundle.image('ghcr.io/acme/api').tag }}
      - key: replicas
        value: 3
```

`key` is a dotted path with optional `[n]` indices — `spec.template[0].image` —
and a dot inside a key is escaped: `metadata.annotations.flux\.io/x`.

Only the scalar's own span in the file changes; comments, quoting, indentation
and every other line are byte-identical afterwards. Fields are updated, never
created: a missing key is an error naming the path.

JSON files work too — JSON is YAML, and a scalar replaced in place leaves a JSON
document valid JSON. There is no separate `edit-json` step.

Strings are quoted only where leaving them bare would change their type, which
includes the YAML 1.1 booleans (`yes`, `on`, `no`, `off`) because Flux and
kustomize read these files with a 1.1 parser.

**Output:** `changed`, `file`.

**Reasons:** `FileNotFound`, `KeyNotFound`, `EditFailed`, `InvalidConfig`.

## render-kustomize

Builds a kustomization and writes the result into the checkout, for the
rendered-manifests pattern.

```yaml
- uses: render-kustomize
  with:
    path: repo/apps/production
    out: repo/rendered/production.yaml
```

| field | type | default |
|---|---|---|
| `path` | string | required — the kustomization directory |
| `out` | string | required — one file, not a directory |
| `loadRestrictionsNone` | bool | `false` |

Run it before `git-commit`, so what lands in git is final state and Flux applies
without rendering anything itself ([D36](DECISIONS.md)). A reviewer then sees
the manifests that will exist rather than a kustomization whose effect they have
to imagine.

Output is deterministic and unchanged output is not rewritten, so a re-run of a
crossing leaves the tree clean. `loadRestrictionsNone` lets the build reference
files outside its own directory; off by default, matching kustomize and Flux.

**Output:** `changed`, `file`, `resources`.

**Reasons:** `RenderFailed`, `NothingRendered`, `FileNotFound`, `InvalidConfig`.

## render-helm

Templates a chart and writes the result into the checkout.

```yaml
- uses: render-helm
  with:
    chart: repo/charts/api
    out: repo/rendered/production.yaml
    releaseName: api
    valuesFiles: [repo/values/production.yaml]
    values: { image: { tag: "${{ bundle.image('ghcr.io/acme/api').tag }}" } }
```

| field | type | default |
|---|---|---|
| `chart` | string | required — a directory in the checkout |
| `out` | string | required |
| `releaseName` | string | required |
| `valuesFiles` | \[string] | — applied in order |
| `values` | object | — applied last, so it overrides a file |
| `namespace` | string | the Gate's namespace |
| `includeCRDs` | bool | `false`, matching `helm template` |
| `kubeVersion` | string | Helm's default, **not the cluster's** |
| `apiVersions` | \[string] | — |

Templating only: it never contacts a cluster, installs anything, or reads
release history. `.Capabilities` comes from configuration rather than from
wherever the controller happens to run, so the same commit renders the same
bytes anywhere — which is what makes the output safe to commit.

The chart is a **directory in the checkout**, not a repository reference: what
is rendered has to be what was reviewed, and a chart pulled at render time could
differ between the pull request and the crossing.

`releaseName` is required rather than defaulted, because chart templates
routinely name resources after it and a guess would silently name everything
after something arbitrary.

**Output:** `changed`, `file`.

**Reasons:** `RenderFailed`, `NothingRendered`, `FileNotFound`, `InvalidConfig`.

## oci-push / oci-pull

For a fleet whose source of truth is an OCI registry rather than git.

```yaml
- uses: oci-push
  with:
    path: repo/rendered
    repo: ghcr.io/acme/manifests
    tag: "${{ bundle.image('ghcr.io/acme/api').tag }}"
    source: https://github.com/acme/fleet
    revision: ${{ steps.commit.sha }}
    credentialsRef: { name: registry }
```

| `oci-push` | type | default |
|---|---|---|
| `path` | string | required — the directory to package |
| `repo`, `tag` | string | required |
| `source`, `revision` | string | recorded as annotations |
| `insecure` | bool | `false` |
| `credentialsRef` | `{name}` | ambient credentials |

| `oci-pull` | type | default |
|---|---|---|
| `repo` | string | required |
| `tag` **or** `digest` | string | one of them, not both |
| `out` | string | required |

The artifact is the one Flux's `OCIRepository` consumes — the media types were
read from an artifact `flux push artifact` produced, not from documentation. Its
`revision` annotation is what Flux reports as the revision it applied, so that
is how a deployment traces back to what produced it.

**The digest is deterministic**: timestamps come from the Passage and tar entries
are walked in order, so re-running a crossing republishes the identical digest
rather than a new revision for Flux to deploy ([D37](DECISIONS.md)).

`oci-pull` replaces its target directory rather than merging, so a retry after a
partial unpack cannot leave a mixture of two artifacts, and an entry naming `../`
is refused rather than quietly relocated.

`insecure` allows a plain-HTTP registry and is always opt-in. Loopback registries
are already treated as insecure by the underlying library, so the option is for
named hosts.

**Output (push):** `repo`, `tag`, `digest`, `url`.
**Output (pull):** `digest`, `out`, `files`.

**Reasons:** `RegistryAuthFailed`, `RegistryFailed`, `FileNotFound`, `InvalidConfig`.

## flux-reconcile

Asks Flux to sync now rather than at its next interval.

```yaml
- uses: flux-reconcile
  with:
    resources:
      - { kind: GitRepository, name: fleet, namespace: flux-system }
```

The annotation Flux watches is a doorbell, not a desired state: it changes
nothing about what Flux will do, only when. Without it a promotion's visible
latency is the source interval ([D26](DECISIONS.md)). The stamp comes from the
Passage, so a requeue rings nothing.

Referring to another namespace is refused unless the controller runs with
`--no-cross-namespace-refs=false`.

**Output:** `requestedAt`, `resources`.

**Reasons:** `FluxPatchFailed`, `InvalidConfig`.

## flux-wait

Blocks the Passage until Flux has applied what the Passage pushed.

| field | type | default |
|---|---|---|
| `resources` | \[`{kind, name, namespace, apiVersion}`] | required |
| `expectedRevision` | string | any |
| `failAfter` | duration | `10m` |

This is the join between Hecate and the delivery engine. Hecate never tells Flux
what to apply; it reads Flux's own status to learn whether the write took effect
([D3](DECISIONS.md)). Wire `expectedRevision` from an earlier step —
`${{ steps.commit.sha }}` — or a stale `Ready=True` from the previous revision
reads as success.

The resources it waited on are handed to the Gate, which keeps watching them
after the Passage ends ([D20](DECISIONS.md)).

**Reasons:** `FluxDegraded`, `InvalidConfig`.

## evidence-gate

Consults [Fides](https://github.com/olafkfreund/evidance-vault) before a
crossing proceeds. The Gate must carry `evidence.fidesEnvironment` and
`evidence.credentialsRef` ([D27](DECISIONS.md)).

```yaml
- uses: evidence-gate
  with:
    gates: [assert, allowlist, policy, change]
    policy: production-baseline
    maxRisk: 40
    holdTimeout: 48h
```

| field | type | default |
|---|---|---|
| `gates` | \[assert \| allowlist \| policy \| change] | required |
| `policy` | string | every policy that applies |
| `maxRisk` | 0–100 | none |
| `reportArtifacts` | bool | `false` |
| `holdTimeout` | duration | `24h` |
| `pollInterval` | duration | `1m` |

| gate | asks | scoped by |
|---|---|---|
| `assert` | is this artifact compliant with a policy? | the Bundle's image digests |
| `allowlist` | is this artifact approved for this environment? | digest + environment |
| `policy` | does the environment's policy accept this build? | environment + the build's trail |
| `change` | evidence-backed approval verdict and 0–100 risk score | the build's trail |

The `change` gate also reports **who is deploying**. Before it reads the verdict
it records the crossing's actor on the trail as the deployer, which is the third
identity Fides compares when it evaluates segregation of duties — the other two
being the committer CI recorded and the approver `hecate approve` recorded. All
three must be distinct people.

An automatic crossing records nobody: Hecate is not a person, and naming it
would let a change pass four-eyes with two humans and a robot. The gate then
holds on "no deployer recorded", which is the correct answer for a control that
requires a human to deploy.


`reportArtifacts: true` records every image digest in the Bundle against the
trail, so a change gate judges the whole release rather than only the image
whose trail was looked up.

**Off by default, and worth understanding before turning on.** Fides upserts on
the digest and overwrites the trail link, so reporting asserts that the other
images belong to *this* trail — true when one CI run built them all, wrong when
it did not, and only you know which. For the same reason nothing is ever
reported before the trail is resolved: a report with no trail would null the
link CI made and detach the very evidence being judged.


The trail is the one **CI** recorded when it built the image, found from the
digest — not one Hecate opened. A fresh trail would carry none of the SBOM and
scan attestations the last two gates judge, so both would refuse every promotion
([D29](DECISIONS.md)). An artifact Fides has never seen is a refusal: approving
what cannot be seen is the failure the system exists to prevent.

**A `hold` waits, a refusal does not** ([D30](DECISIONS.md)). The first three
gates end the crossing when they say no — the answer will not change until a
human changes something. A held change gate reports `Running` and polls, because
a hold usually means every control passed and Fides wants a second signature.
The wait is bounded by `holdTimeout`. `maxRisk` is terminal even when Fides
approved, since the score is about evidence that already exists.

An unreachable Fides is retryable; a rejected token is not.

**Output:** `trail`, `verdict`, `risk`. Also written to
`passage.status.evidence`, which is what `hecate verify` reads.

Checked against a live Fides with `make fides-test`; see
[ONBOARDING](ONBOARDING.md).

**Reasons:** `NotCompliant`, `NotAllowlisted`, `NoEvidence`, `ChangeHeld`,
`EvidenceUnavailable`, `InvalidConfig`.

## http

Calls an external system: notify, trigger, query.

```yaml
- uses: http
  with:
    url: https://change.acme.io/api/windows/current
    secretHeaders:
      - name: Authorization
        prefix: "Bearer "
        secretRef: { name: change-api }
    successIf: json.state == "open"
```

| field | type | default |
|---|---|---|
| `url` | string | required, http or https |
| `method` | string | `GET`, or `POST` with a body |
| `headers` | map | — |
| `secretHeaders` | \[`{name, prefix, secretRef, key}`] | `key` defaults to `token` |
| `body` | any | sent as JSON |
| `expectStatus` | \[int] | any 2xx |
| `successIf` | expression | — |
| `timeout` | duration | `30s` |

**Success is defined, not assumed.** Plenty of systems answer 200 with a body
saying the request failed. `successIf` is a condition over `status`, `body` and
parsed `json`, evaluated after the response arrives — so it takes no `${{ }}`,
because those are substituted before the step runs and there is no response then.

**Put nothing secret in `headers`.** A Gate is an ordinary object that anyone
with read access to the namespace can read. `secretHeaders` are resolved at call
time and applied last.

A 4xx fails terminally; a 5xx or a timeout stays retryable. The captured body is
capped at 4KB and reports `truncated`.

**Output:** `status`, `body`, `truncated`, and `json` when the body parses.

**Reasons:** `HTTPFailed`, `HTTPUnreachable`, `InvalidConfig`.

---

## Not here yet

The step library is complete. New steps are added when something needs one, not
in anticipation.
