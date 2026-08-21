# Signing in

Hecate's web UI authenticates with OIDC, and adds no identity model of its own.
That sentence is the whole design and it has one consequence that decides
whether your deployment will work:

> **The cluster must trust the same issuer.**

Kubernetes can be configured to trust an OIDC issuer, and when it is, an ID
token from that issuer *is* a valid Kubernetes bearer token. So Hecate does not
verify who you are and then decide what you may do. It obtains a credential the
cluster already understands and keeps asking the API server the same questions
it asks a `kubectl` user (D32, and [Permissions](RBAC.md)).

Configure Hecate against an issuer the cluster does not trust and **every login
will succeed and every request afterwards will be refused**. The token is real;
the cluster simply has no reason to believe it. Hecate cannot detect this at
startup, because whether the API server trusts an issuer is not something it
exposes — so it says so in the flag's help rather than letting it be discovered
one confused user at a time.

## What to configure

Read from the environment, because a client secret on a command line is visible
in `ps` and in every process listing.

| Variable | Meaning |
|---|---|
| `HECATE_OIDC_ISSUER` | issuer URL, exactly as the provider publishes it |
| `HECATE_OIDC_CLIENT_ID` | the client you registered |
| `HECATE_OIDC_CLIENT_SECRET` | its secret |
| `HECATE_OIDC_REDIRECT_URL` | `https://<your-hecate>/auth/callback` |
| `HECATE_OIDC_SCOPES` | defaults to `profile,email,groups` |

And on the API server, the matching half:

```
--oidc-issuer-url=<the same issuer>
--oidc-client-id=<the same client id>
--oidc-username-claim=email
--oidc-groups-claim=groups
```

Two of those must be *identical strings*, not merely equivalent URLs: the issuer
and the client ID. A trailing slash on one side is a mismatch.

`kube-apiserver` reads these at startup, so on a managed cluster this is a
control-plane setting rather than something you can add later from inside.

## The claims decide who you are to Kubernetes

`--oidc-username-claim=email` means an RBAC binding names the user by email:

```yaml
subjects:
  - kind: User
    name: olaf@acme.example      # the email claim, verbatim
```

With `--oidc-groups-claim=groups`, a provider group becomes a Kubernetes group,
which is usually what you want — bind `hecate-approver` to the group your
organisation already manages rather than to people one at a time.

**A login with no binding is a working login that can do nothing.** That is
correct behaviour and it looks like a bug, so see the diagnosis below.

## Diagnosing a failed sign-in

The status code tells you which half is wrong, and they are worth telling apart:

| | Means | Fix |
|---|---|---|
| **401** on every request after login | the cluster did not accept the token at all | the API server does not trust this issuer, or the issuer/client-id strings differ |
| **403** naming your identity | the cluster knows exactly who you are and RBAC refuses | bind a role — see [Permissions](RBAC.md) |
| Login loops back to the provider | the redirect URL does not match what is registered | make them identical, including scheme and port |
| `initializing plugin: ... EOF` at startup | Hecate cannot reach the issuer | check the network path, and `SSL_CERT_FILE` for a private CA |

A 403 is the good failure: it means every layer worked and the only thing left
is a role binding. It names the identity the cluster resolved, which is also how
you check the username claim is doing what you expect:

```console
$ curl -H "Authorization: Bearer $TOKEN" https://hecate.acme.example/api/v1alpha1/namespaces/prod/gates
{"error":"system:serviceaccount:default:default may not list gates in prod","status":"forbidden"}
```

## Trying it locally

The repository ships a working provider — Dex, installed into the dev cluster
with the API server configured to trust it, which is the only way to exercise
browser sign-in without a real IdP:

```console
$ HECATE_OIDC=1 make cluster     # must be at creation: kube-apiserver reads OIDC flags at startup
$ make oidc-check
==> sign-in works: Dex issued a token, the cluster accepted it, the API answered 200
```

That check is not a smoke test of the login page. It drives the whole chain —
Dex issues a token, the cluster accepts it, the API answers 200 — so a change
that breaks any layer fails it. The full walkthrough, including the self-signed
CA, is in [Onboarding](ONBOARDING.md#signing-in-to-the-web-portal).

## Provider recipes

### Dex on EKS

Written from the deployment it describes rather than from documentation, which
is the bar this section sets for itself. Every value below was read back out of
a cluster people sign into.

**1. Dex.** The client id and the redirect URI have to match what Hecate is
configured with, exactly — a trailing slash is a different URI.

```yaml
issuer: https://dex.example.com
storage:
  type: kubernetes          # see the warning below; `memory` will lock you out
  config:
    inCluster: true
oauth2:
  skipApprovalScreen: true  # a consent screen for your own tool is a click nobody learns from
staticClients:
  - id: hecate
    name: Hecate
    secret: <the same value as Hecate's clientSecret Secret>
    redirectURIs:
      - https://hecate.example.com/auth/callback
```

**2. The cluster.** Kubernetes has to trust the same issuer, or a token Dex
happily issues is one the API server has never heard of. On EKS:

One `--oidc` value, on one line: it is a comma-separated structure, and a
backslash continuation inside it joins only while the trailing backslash
survives whatever edits the line on its way to a terminal.

```console
$ aws eks associate-identity-provider-config --cluster-name <cluster> --region <region> \
    --oidc 'identityProviderConfigName=dex,issuerUrl=https://dex.example.com,clientId=hecate,usernameClaim=email,groupsClaim=groups'
```

`clientId` is the same `hecate` Dex knows and Hecate presents. It is not a
separate registration: the cluster validates the token's `aud`, and a different
value here rejects every token with an error that names none of this.

Association takes around fifteen minutes. Confirm it rather than assuming:

```console
$ aws eks describe-identity-provider-config --cluster-name <cluster> \
    --identity-provider-config type=oidc,name=dex \
    --query 'identityProviderConfig.oidc.status'
"ACTIVE"
```

**3. Hecate.**

```yaml
api:
  oidc:
    enabled: true
    issuer: https://dex.example.com
    clientID: hecate
    redirectURL: https://hecate.example.com/auth/callback
    clientSecretRef:
      name: hecate-oidc     # a Secret whose `clientSecret` key holds it
      # key: clientSecret   # the default; set it if your Secret names it otherwise
```

**4. Grant somebody something.** Signing in gets a token the cluster trusts and
nothing else — see [RBAC.md](RBAC.md). With `usernameClaim=email` the subject is
the email address, so a binding names `promoter@example.com` and not `promoter`.

#### `storage.type: memory` will lock every user out

Dex with `memory` storage generates a fresh signing keypair on every restart,
and a restart is what a Helm upgrade, a node eviction or a Spot reclaim does to
it. **EKS caches the provider's JWKS when the config is associated and does not
re-fetch it**, so the new keys are ones the cluster will not accept. Every login
then fails with a signature error naming no cause, and waiting does not fix it.

Recovering means `disassociate-identity-provider-config` and associating again
— about twenty-five minutes, with every login failing throughout. Using
`kubernetes` storage keeps the keys across restarts and the problem never
arises. This is the single most expensive mistake available on this page.

### Okta, Entra, Google, Keycloak

**Not written yet, deliberately**, and for the reason above: a recipe written
from documentation rather than from a working deployment is how a project ends
up with instructions that are confidently wrong — which is the problem recipes
exist to solve, made worse by carrying our name.

The configuration in this page is provider-agnostic and is what all of them
need; what a recipe adds is the specific clicking-through. Tracked in #52;
contributions from anyone running one are welcome, and the useful contribution
is the diff between this page and what you actually had to do.
