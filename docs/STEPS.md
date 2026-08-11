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

**Output:** `sha`, `committed`.

**Reasons:** `WorkDirLost`, `GitFailed`, `InvalidConfig`.

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

**Credentials:** an API token under `token`, or `password`.

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

`render-kustomize` and `render-helm` ([#67]), `oci-push` and `oci-pull`
([#69]), and per-step schema validation at admission ([#71], [#97]). Until that
last one lands, a mistake in a `with:` block is caught when the step runs, as a
terminal `InvalidConfig` naming the field.

[#67]: https://github.com/olafkfreund/Hecate/issues/67
[#69]: https://github.com/olafkfreund/Hecate/issues/69
[#71]: https://github.com/olafkfreund/Hecate/issues/71
[#97]: https://github.com/olafkfreund/Hecate/issues/97
