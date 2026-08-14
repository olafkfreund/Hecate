"use client";

import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, XCircle } from "lucide-react";

import { api, ApiError, Settings, Grant } from "@/lib/api";
import { useQueryParam } from "@/lib/browser";

/**
 * Settings shows what is configured and lets it be changed.
 *
 * Everything on the left is read from cluster state rather than from the
 * chart's values: values say what someone intended, a Gate says what is in
 * force, and when they disagree the useful answer is the one the controller
 * will act on.
 *
 * Everything on the right writes to the cluster, and two of those writes carry
 * a warning the screen makes plainly rather than burying:
 *
 *   - a Gate edited here is restored by Flux on its next sync if that Gate
 *     comes from git, which it usually does
 *   - "adding a user" creates a ClusterRoleBinding, because Hecate has no user
 *     store; the people themselves live in your identity provider
 */
export default function SettingsPage() {
  const namespace = useQueryParam("namespace", "default");
  const [settings, setSettings] = useState<Settings | null>(null);
  const [grants, setGrants] = useState<Grant[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);

  // Every setState here happens after an await, never on the synchronous path
  // an effect runs on. Calling a function that sets state synchronously from an
  // effect makes React render again before the first render has committed, and
  // the lint rule that catches it is right to: the version of this that did so
  // was one thrown error away from a render loop.
  const refresh = useCallback(async () => {
    try {
      const s = await api.settings();
      setSettings(s);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    try {
      const g = await api.grants();
      setGrants(g.grants);
    } catch (e) {
      // A 403 here is the ordinary case for someone who may use Hecate but not
      // administer it, so it leaves the section absent rather than shouting.
      if (!(e instanceof ApiError && e.status === 403)) {
        setError(e instanceof Error ? e.message : String(e));
      }
      setGrants(null);
    }
  }, []);

  // Promise callbacks rather than a call in the effect body. The lint rule
  // objects to the latter because it cannot see that every setState is behind
  // an await, and it is right to insist: a synchronous set during an effect
  // renders again before the first render has committed. This is the shape the
  // namespace picker in the shell already uses.
  //
  // `live` drops results that arrive after the page has gone, which is a real
  // case here — the settings call probes every connected cluster before it
  // answers, so it can outlive a quick navigation away.
  useEffect(() => {
    let live = true;
    api
      .settings()
      .then((s) => live && setSettings(s))
      .catch((e) => live && setError(e instanceof Error ? e.message : String(e)));
    api
      .grants()
      .then((g) => live && setGrants(g.grants))
      .catch((e) => {
        if (!live) return;
        // A 403 is the ordinary case for someone who may use Hecate but not
        // administer it, so the section is absent rather than shouting.
        if (!(e instanceof ApiError && e.status === 403)) {
          setError(e instanceof Error ? e.message : String(e));
        }
        setGrants(null);
      });
    return () => {
      live = false;
    };
  }, []);

  return (
    <div className="space-y-8">
      <header>
        <h1 className="text-2xl font-semibold">Settings</h1>
        <p className="text-[var(--muted-foreground)]">
          What this installation is connected to, and who may use it.
        </p>
      </header>

      {error && (
        <p className="rounded-md border border-red-500/40 bg-red-500/10 p-3 text-sm">{error}</p>
      )}
      {note && (
        <p className="rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm">{note}</p>
      )}

      <Section title="This installation">
        <Row label="Version" value={settings?.version ?? "…"} />
        <Row label="Signed in as" value={settings?.identity.name ?? "…"} />
        {settings?.identity.groups?.length ? (
          <Row label="Groups" value={settings.identity.groups.join(", ")} />
        ) : null}
        <Row
          label="Traces"
          value={
            settings?.telemetry.configured
              ? settings.telemetry.endpoint ?? "configured"
              : "not configured"
          }
          hint="Hecate exports spans and stores none — open your collector to read them."
        />
      </Section>

      <Section title="Evidence servers">
        {settings?.fides.length ? (
          settings.fides.map((f) => (
            <div key={f.serverURL} className="space-y-1 border-b border-[var(--border)] py-3 last:border-0">
              <div className="flex items-center gap-2">
                {f.reachable ? (
                  <CheckCircle2 className="size-4 text-green-500" aria-label="reachable" />
                ) : (
                  <XCircle className="size-4 text-red-500" aria-label="unreachable" />
                )}
                <code className="text-sm">{f.serverURL}</code>
              </div>
              {!f.reachable && f.detail && (
                <p className="text-sm text-red-400">{f.detail}</p>
              )}
              <p className="text-sm text-[var(--muted-foreground)]">
                used by {f.gates.join(", ")}
              </p>
              {f.environments?.length ? (
                <p className="text-sm text-[var(--muted-foreground)]">
                  environments {f.environments.join(", ")}
                </p>
              ) : null}
            </div>
          ))
        ) : (
          <Empty>No Gate names an evidence server.</Empty>
        )}
        <EvidenceForm
          namespace={namespace}
          onDone={(msg) => {
            setNote(msg);
            void refresh();
          }}
          onError={setError}
        />
      </Section>

      <Section title="Connected clusters">
        {settings?.clusters.length ? (
          settings.clusters.map((c) => (
            <div key={c.secret} className="space-y-1 border-b border-[var(--border)] py-3 last:border-0">
              <div className="flex items-center gap-2">
                {c.reachable ? (
                  <CheckCircle2 className="size-4 text-green-500" aria-label="reachable" />
                ) : (
                  <XCircle className="size-4 text-red-500" aria-label="unreachable" />
                )}
                <code className="text-sm">{c.secret}</code>
              </div>
              {!c.reachable && c.detail && <p className="text-sm text-red-400">{c.detail}</p>}
              <p className="text-sm text-[var(--muted-foreground)]">
                {c.gates.length ? (
                  `watched by ${c.gates.join(", ")}`
                ) : (
                  // Connected but unused is a normal state, not an error — and
                  // saying so is the difference between this screen and the one
                  // that listed only Gate-referenced clusters, where a freshly
                  // stored kubeconfig simply never appeared.
                  <>
                    connected, not yet used. Add{" "}
                    <code>clusterRef: {`{name: ${c.secret.split("/")[1]}}`}</code> to a Gate&apos;s
                    watch or flux-wait step.
                  </>
                )}
              </p>
            </div>
          ))
        ) : (
          <Empty>No clusters connected, and no Gate watches a remote one.</Empty>
        )}
        <ClusterForm
          namespace={namespace}
          onDone={(msg) => {
            setNote(msg);
            void refresh();
          }}
          onError={setError}
        />
      </Section>

      {grants !== null && (
        <Section title="Who may use Hecate">
          <p className="pb-2 text-sm text-[var(--muted-foreground)]">
            Hecate keeps no users of its own. These are ClusterRoleBindings, and the people
            they name come from your identity provider — adding one here grants a role, it
            does not create an account.
          </p>
          {grants.length ? (
            grants.map((g) => (
              <Row
                key={g.binding + g.subject}
                label={`${g.subject} (${g.kind})`}
                value={g.role}
                hint={g.grantedBy ? `granted by ${g.grantedBy}` : undefined}
              />
            ))
          ) : (
            <Empty>Nobody holds a Hecate role yet.</Empty>
          )}
          <GrantForm
            onDone={(msg) => {
              setNote(msg);
              void refresh();
            }}
            onError={setError}
          />
        </Section>
      )}
    </div>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-lg border border-[var(--border)] p-4">
      <h2 className="pb-2 text-lg font-medium">{title}</h2>
      {children}
    </section>
  );
}

function Row({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-2 border-b border-[var(--border)] py-2 last:border-0">
      <span className="text-sm text-[var(--muted-foreground)]">{label}</span>
      <span className="text-sm">{value}</span>
      {hint && <span className="w-full text-xs text-[var(--muted-foreground)]">{hint}</span>}
    </div>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="py-2 text-sm text-[var(--muted-foreground)]">{children}</p>;
}

const field =
  "rounded-md border border-[var(--border)] bg-transparent px-2 py-1 text-sm text-[var(--foreground)]";
const button =
  "rounded-md border border-[var(--border)] px-3 py-1 text-sm hover:bg-[var(--secondary)] disabled:opacity-50";

/** EvidenceForm points a Gate at an evidence server. */
function EvidenceForm({
  namespace,
  onDone,
  onError,
}: {
  namespace: string;
  onDone: (msg: string) => void;
  onError: (msg: string) => void;
}) {
  const [gate, setGate] = useState("");
  const [serverURL, setServerURL] = useState("");
  const [environment, setEnvironment] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <form
      className="mt-4 space-y-2 border-t border-[var(--border)] pt-4"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        try {
          const r = await api.setEvidence(namespace, gate, {
            serverURL,
            fidesEnvironment: environment,
          });
          onDone(r.note);
        } catch (err) {
          onError(err instanceof Error ? err.message : String(err));
        } finally {
          setBusy(false);
        }
      }}
    >
      <p className="flex items-start gap-2 text-sm text-amber-400">
        <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
        <span>
          This edits the Gate in the cluster. If it is reconciled from git, Flux restores the
          committed value on the next sync — change it there to make it stick.
        </span>
      </p>
      <div className="flex flex-wrap gap-2">
        <input
          className={field}
          placeholder={`Gate in ${namespace}`}
          value={gate}
          onChange={(e) => setGate(e.target.value)}
          aria-label="Gate"
          required
        />
        <input
          className={`${field} min-w-64`}
          placeholder="https://fides.example.com"
          value={serverURL}
          onChange={(e) => setServerURL(e.target.value)}
          aria-label="Fides server URL"
        />
        <input
          className={`${field} min-w-72`}
          placeholder="environment UUID"
          value={environment}
          onChange={(e) => setEnvironment(e.target.value)}
          aria-label="Fides environment"
          required
        />
        <button className={button} disabled={busy} type="submit">
          {busy ? "Applying…" : "Apply"}
        </button>
      </div>
    </form>
  );
}

/** ClusterForm stores a remote cluster's kubeconfig. */
function ClusterForm({
  namespace,
  onDone,
  onError,
}: {
  namespace: string;
  onDone: (msg: string) => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState("");
  const [kubeconfig, setKubeconfig] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <form
      className="mt-4 space-y-2 border-t border-[var(--border)] pt-4"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        try {
          const r = await api.connectCluster(namespace, name, kubeconfig);
          onDone(
            `${r.secret} ${r.created ? "created" : "updated"}. Reference it from a Gate's watch as clusterRef.`,
          );
          setKubeconfig("");
        } catch (err) {
          onError(err instanceof Error ? err.message : String(err));
        } finally {
          setBusy(false);
        }
      }}
    >
      <div className="flex flex-wrap gap-2">
        <input
          className={field}
          placeholder={`Secret name in ${namespace}`}
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-label="Cluster secret name"
          required
        />
        <button className={button} disabled={busy} type="submit">
          {busy ? "Saving…" : "Connect"}
        </button>
      </div>
      <textarea
        className={`${field} h-28 w-full font-mono`}
        placeholder="paste the kubeconfig for the remote cluster"
        value={kubeconfig}
        onChange={(e) => setKubeconfig(e.target.value)}
        aria-label="Kubeconfig"
        required
      />
      <p className="text-xs text-[var(--muted-foreground)]">
        Stored under the key <code>value</code>, the same convention Flux uses. A Gate still
        only sees its own namespace on the remote cluster — a cluster reference is not a way
        around the tenant rule.
      </p>
    </form>
  );
}

/** GrantForm binds someone to a Hecate role. */
function GrantForm({
  onDone,
  onError,
}: {
  onDone: (msg: string) => void;
  onError: (msg: string) => void;
}) {
  const [subject, setSubject] = useState("");
  const [kind, setKind] = useState("User");
  const [role, setRole] = useState("hecate-viewer");
  const [busy, setBusy] = useState(false);

  return (
    <form
      className="mt-4 flex flex-wrap gap-2 border-t border-[var(--border)] pt-4"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        try {
          const r = await api.grant(subject, kind, role);
          onDone(`${r.binding} ${r.created ? "created" : "already existed"}.`);
          setSubject("");
        } catch (err) {
          onError(err instanceof Error ? err.message : String(err));
        } finally {
          setBusy(false);
        }
      }}
    >
      <input
        className={`${field} min-w-64`}
        placeholder="someone@example.com"
        value={subject}
        onChange={(e) => setSubject(e.target.value)}
        aria-label="Subject"
        required
      />
      <select className={field} value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Kind">
        <option value="User">User</option>
        <option value="Group">Group</option>
      </select>
      <select className={field} value={role} onChange={(e) => setRole(e.target.value)} aria-label="Role">
        <option value="hecate-viewer">hecate-viewer</option>
        <option value="hecate-promoter">hecate-promoter</option>
        <option value="hecate-approver">hecate-approver</option>
      </select>
      <button className={button} disabled={busy} type="submit">
        {busy ? "Granting…" : "Grant"}
      </button>
    </form>
  );
}
