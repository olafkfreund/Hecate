# Onboarding

From a clean NixOS machine to a running Hecate with a live test cluster.

Everything is pinned in `flake.nix`. You do not install Go, kubectl, Helm, k3d or
the Flux CLI — the dev shell brings them, at versions that match CI.

---

## 1. Prerequisites

Two things Nix cannot provide for you:

**Flakes enabled.** In your system configuration:

```nix
nix.settings.experimental-features = [ "nix-command" "flakes" ];
```

**A running Docker daemon**, because k3d runs Kubernetes inside it:

```nix
virtualisation.docker.enable = true;
users.users.<you>.extraGroups = [ "docker" ];
```

Log out and back in after adding yourself to `docker`, then check:

```console
$ docker info >/dev/null && echo ok
ok
```

> Podman works for most things but k3d expects Docker. If you are on Podman,
> `virtualisation.docker.enable` alongside it is the path of least resistance.

## 2. Enter the dev shell

```console
$ git clone https://github.com/olafkfreund/Hecate && cd Hecate
$ nix develop
```

First run builds the toolchain and takes a few minutes; afterwards it is instant.

**Optional but recommended — direnv**, so the shell loads on `cd`:

```nix
programs.direnv.enable = true;      # in your NixOS or Home Manager config
```

```console
$ direnv allow
```

The shell pins `KUBECONFIG` to `./.dev/kubeconfig`. That is deliberate: a stray
`kubectl delete` during development can never reach a real cluster.

## 3. Run the tests

```console
$ make test
```

310 tests, about a second, **no cluster required**. That is the standing rule:
anything that can be tested without a cluster must be. If a change makes you
reach for a cluster to test it, that is usually a sign the logic wants
extracting into something pure.

### Against a real Fides

The suite above fakes Fides. The fake was built by reading Fides' handlers,
which is not the same as knowing what it sends — `policy-check` returns
`compliant`, not `passed`, and a client reading the wrong key would refuse every
crossing while looking like it worked.

If you have a Fides to point at, check the client against it:

```console
$ kubectl -n fides port-forward svc/fides-server 18080:8080 &
$ export FIDES_SERVER_URL=http://127.0.0.1:18080
$ export FIDES_TOKEN=$(kubectl -n fides get secret fides-secrets \
    -o go-template='{{index .data "api-token"}}' | base64 -d)
$ make fides-test
```

Every call is a read, so it is safe against a real compliance system — which is
the only useful kind, since a Fides with no history has nothing to verify. The
test discovers an environment and an artifact from the server rather than
hardcoding UUIDs, so it runs against any instance.

It has already earned its place: it found that Fides answers `200` with
`{"valid":true,"count":0}` for a trail that does not exist, which `hecate
verify` was reporting as a verified chain and exit 0.

## 4. Bring up a test cluster

```console
$ make cluster
```

This creates a k3d cluster in Docker, installs Flux into it, applies Hecate's
CRDs, and writes `./.dev/kubeconfig`. It also starts a local container registry
on `localhost:5001` and a git server, so images and commits never leave the
machine.

The git server matters more than it sounds. A promotion *is* a git write, so
without somewhere to push, the deployed controller could only ever be tested
doing the one thing that is not a git write — waiting for Flux. It is seeded
with a `fleet` repository the e2e promotion edits, and Flux reads the same
repository back.

```console
$ kubectl get pods -n flux-system
$ kubectl get beacons,bundles,gates,passages -A
```

Then the end-to-end suite:

```console
$ make e2e
```

This drives a Bundle from discovery through two Gates against the real API
server, and then runs a full promotion through the deployed controller: clone,
repin, edit, commit, push, and wait for Flux to apply that exact commit. It is
what catches CRD drift — a generated CRD that no longer matches the Go types is
invisible to unit tests and fatal in a cluster — and it is the only place the
product's central claim is actually demonstrated rather than asserted.

Tear down with `make cluster-rm`. It costs nothing to recreate.

## 5. The loop

```console
$ make test          # after every change
$ make generate      # after touching api/ or any kubebuilder marker
$ make check         # what CI enforces: gofmt, vet, tests
```

**`make generate` matters.** CRDs and RBAC are generated from the Go types and
the `+kubebuilder:` markers in the controllers. CI fails if the committed output
has drifted, because a stale CRD silently rejects fields the code sets.

To run the controller against your dev cluster:

```console
$ make install    # build the image, push to the local registry, install the chart
$ make uninstall  # remove it again; CRDs are left alone
$ make run        # or run it out-of-cluster against KUBECONFIG, for a debugger
```

`make run` is the faster loop while iterating on controller logic; `make install`
is how you check the packaging, the RBAC and the hardened pod actually work.

### Signing in to the web portal

The portal is served by `hecate-api` itself, so there is one URL and no CORS. It
authenticates the way everything else does: you present a Kubernetes credential
and Hecate asks the cluster who you are and what you may do. A browser has no
kubeconfig, so it needs an OIDC provider **that the cluster also trusts** — and
that second half is what makes a local setup awkward.

`HECATE_OIDC=1` handles it:

```console
$ make cluster-rm
$ HECATE_OIDC=1 make cluster     # installs Dex and configures kube-apiserver
```

It must be at creation time: `kube-apiserver` reads its OIDC flags at startup,
so they cannot be added to a cluster that already exists.

The issuer is `https://dex.<your-ip>.nip.io:5556/dex`. That name is the trick —
it resolves to this machine from the browser, from `hecate-api`, and from inside
the k3d server container, so one URL works for all three. The usual alternative
is an `/etc/hosts` entry, which a setup script has no business writing.

Then run the API with the printed environment and open it:

```console
$ export HECATE_OIDC_ISSUER=https://dex.<your-ip>.nip.io:5556/dex
$ export HECATE_OIDC_CLIENT_ID=hecate
$ export HECATE_OIDC_CLIENT_SECRET=hecate-dev-secret
$ export HECATE_OIDC_REDIRECT_URL=http://127.0.0.1:18099/auth/callback
$ export SSL_CERT_FILE=$PWD/.dev/oidc/ca.crt
$ go run ./cmd/hecate-api -addr 127.0.0.1:18099 -oidc-insecure-cookie
```

Sign in as **olaf@hecate.test / hecate-dev**. The certificate is self-signed, so
the browser objects once.

Everything here is disposable — a throwaway CA, one static password, a client
secret in a ConfigMap. The cluster is offline and holds nothing.

**If sign-in succeeds and every request afterwards is refused**, the cluster is
not trusting the issuer. Check `docker logs k3d-hecate-dev-server-0 | grep oidc`:
`initializing plugin: ... EOF` means the API server cannot reach Dex, which is
the failure this whole arrangement exists to avoid.

### Looking at traces

Tracing is off unless the environment asks for it — the OpenTelemetry SDK's own
default is to export to `localhost:4318`, which would mean a controller logging
connection failures forever about a collector nobody deployed. Every knob is a
standard `OTEL_*` variable and none of them are overridden, so your own
collector configuration works here unchanged.

```console
$ make collector    # a span-printing collector in the dev cluster
$ helm upgrade hecate charts/hecate -n hecate-system --reuse-values \
    --set otel.enabled=true --set otel.endpoint=http://otelcol.hecate-system:4317
$ kubectl logs deploy/otelcol -n hecate-system -f
```

Drive a crossing and you get one trace per Passage — the Passage a root span,
each step a child. The trace is emitted when the Passage finishes rather than
while it runs, because a Passage outlives the process that started it; the
reasoning is [D41](DECISIONS.md).

## 6. Secrets

Encrypted with [agenix](https://github.com/ryantm/agenix) and committed. Nothing
is encrypted yet — `secrets/secrets.nix` declares the recipients and the files
the provider e2e work will need, so the recipient list is settled before the
first secret exists.

**To become a recipient**, add your public key to `secrets/secrets.nix`:

```console
$ cat ~/.ssh/id_ed25519.pub
```

Then have an existing recipient re-encrypt:

```console
$ make secrets-rekey
```

**To create or edit a secret:**

```console
$ make secrets-edit SECRET=github-token.age
```

Three boundaries worth being clear about, because they are easy to conflate:

| | What it is |
|---|---|
| **agenix** | Local development, and NixOS hosts that run Hecate. Encrypted in git. |
| **GitHub Actions** | Repository secrets. CI has no age identity and should not get one — a runner that can decrypt every developer secret is a poor trade for avoiding one settings page. |
| **Hecate at runtime** | Ordinary Kubernetes Secrets, referenced by `credentialsRef`. agenix is how those get *authored* on a NixOS host; the controller knows nothing about it. |

## 7. What to read next

- [ARCHITECTURE.md](ARCHITECTURE.md) — the four resources and why the model is
  shaped this way. Start here.
- [DECISIONS.md](DECISIONS.md) — every non-obvious decision, with its reasoning.
  **Read this before proposing a structural change**; the answer is often
  already there, including for things we deliberately rejected.
- [DEVELOPMENT-PLAN.md](DEVELOPMENT-PLAN.md) — what lands when, the support
  matrix, and how we track Flux upstream.

## Troubleshooting

**`docker info` fails.** The daemon is not running or you are not in the
`docker` group. `systemctl status docker`, and log out and back in after a group
change — a new shell is not enough.

**`make cluster` hangs at "waiting for the API server".** Usually port 6443 or
5001 is already taken by an earlier cluster. `k3d cluster list`, then
`make cluster-rm`.

**`make e2e` says no cluster reachable.** `KUBECONFIG` is set by the dev shell.
If you invoked make from outside it: `export KUBECONFIG=$PWD/.dev/kubeconfig`.

**CI fails on "Generated files are current".** You changed `api/` or a
kubebuilder marker without running `make generate`. Run it and commit the diff.

**`helm install` fails with a CRD conflict.** Something applied the CRDs
client-side first. `make cluster` uses server-side apply for exactly this
reason; if you applied them by hand, repeat it with
`kubectl apply --server-side --force-conflicts`.

**A new API field is silently ignored in the cluster.** Helm installs `crds/`
once and never updates it on upgrade, so the API server prunes a field its CRD
does not declare — no error, just a zero value in the controller. `make install`
applies the CRDs explicitly for this reason; if you are upgrading a real
installation, `kubectl apply --server-side -f charts/hecate/crds/` first. See
[#117](https://github.com/olafkfreund/Hecate/issues/117).

**`nix build` fails on a vendor hash mismatch.** A dependency changed. Nix
prints the correct hash — put it in `flake.nix` as `vendorHash`.

**...but it passed locally and failed in CI.** Expected, and worth knowing: Nix
treats a fixed-output derivation as already realised when an output with the
specified hash exists in the store, so a stale `vendorHash` can validate against
a leftover result on your machine. Only a clean store catches it, which is what
the Nix workflow is for. Trust CI over a local pass here.

**The trigger is not "adding a dependency".** That is what this page used to
say, and it is wrong in a way that broke the build three times. `vendorHash`
covers the set of modules actually *downloaded*, which follows the import graph
— so importing a new package from a module already in `go.mod` moves the hash
while `go.mod` and `go.sum` stay byte-identical. `k8s.io/client-go/util/retry`
did exactly that.

**So `make check` verifies it for you**, and `make flake-hash` prints the value
to paste into `flake.nix`. Do not reason about whether the hash *could* have
changed: the check is cheaper than the reasoning, and it is right.
