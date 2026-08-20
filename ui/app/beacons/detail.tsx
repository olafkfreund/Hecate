"use client";

import { useState } from "react";
import Link from "next/link";
import { ArrowLeft, Loader2, RefreshCw } from "lucide-react";
import { api, ApiError, Unauthenticated, type Beacon } from "@/lib/api";
import { Panel, useLiveApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { describeWatch } from "./page";

/** One Beacon: what it watches, what it last saw, and a way to make it look now. */
export function BeaconDetail() {
  const ns = useNamespace();
  const name = useQueryParam("name", "");
  const [polled, setPolled] = useState(0);
  const state = useLiveApi(() => api.beacon(ns, name), [ns, name, polled]);

  return (
    <div>
      <Link
        href="/beacons/"
        className="mb-4 inline-flex items-center gap-1.5 text-sm text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
      >
        <ArrowLeft size={14} aria-hidden />
        Beacons
      </Link>

      <Panel state={state}>
        {(b: Beacon) => (
          <div className="space-y-8">
            <header>
              <div className="flex items-baseline gap-3">
                <h1 className="text-xl font-semibold tracking-tight">{b.metadata.name}</h1>
                {b.spec.suspend && (
                  <span className="text-sm text-[var(--unknown)]">suspended</span>
                )}
              </div>
              <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                {b.spec.interval ? `Looks every ${b.spec.interval}.` : "Looks on the default interval."}
                {b.spec.emit && ` Emits ${b.spec.emit}.`}
              </p>
              {b.status?.latestBundle && (
                <p className="mt-2 text-sm">
                  Latest Bundle:{" "}
                  <Link
                    href={{
                      pathname: "/bundles/",
                      query: { name: b.status.latestBundle, namespace: ns },
                    }}
                    className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                  >
                    {b.status.latestBundle}
                  </Link>
                </p>
              )}
              {b.status?.lastPolled && (
                <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                  Last looked <time dateTime={b.status.lastPolled}>{when(b.status.lastPolled)}</time>
                </p>
              )}
            </header>

            <section>
              <h2 className="text-sm font-medium">Watching</h2>
              {(b.spec.watch ?? []).length === 0 ? (
                <p className="mt-2 text-sm text-[var(--muted-foreground)]">
                  Nothing. This Beacon will never emit a Bundle.
                </p>
              ) : (
                <ul className="mt-2 space-y-1.5 text-sm">
                  {(b.spec.watch ?? []).map((w, i) => (
                    <li key={i} className="flex gap-3">
                      <span className="w-16 shrink-0 font-mono text-xs text-[var(--muted-foreground)]">
                        {kindOf(w)}
                      </span>
                      <span className="break-all">{describeWatch(w)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            {b.status?.conditions?.length ? (
              <section>
                <h2 className="text-sm font-medium">Conditions</h2>
                <ul className="mt-2 space-y-1.5 text-sm">
                  {b.status.conditions.map((c) => (
                    <li key={c.type} className="flex gap-3">
                      <span className="font-medium">{c.type}</span>
                      <span className="text-[var(--muted-foreground)]">
                        {c.status === "True" ? "yes" : "no"}
                        {c.reason && ` · ${c.reason}`}
                      </span>
                      {c.message && (
                        <span className="text-[var(--muted-foreground)]">{c.message}</span>
                      )}
                    </li>
                  ))}
                </ul>
              </section>
            ) : null}

            <Poll namespace={ns} beacon={b} onPolled={() => setPolled((n) => n + 1)} />
          </div>
        )}
      </Panel>
    </div>
  );
}

function kindOf(w: { image?: unknown; chart?: unknown; git?: unknown; provider?: unknown }): string {
  if (w.image) return "image";
  if (w.chart) return "chart";
  if (w.git) return "git";
  if (w.provider) return "provider";
  return "—";
}

function Poll({
  namespace,
  beacon,
  onPolled,
}: {
  namespace: string;
  beacon: Beacon;
  onPolled: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  const [asked, setAsked] = useState<string | null>(null);

  async function poll() {
    setBusy(true);
    setFailed(null);
    try {
      const { requestedAt } = await api.poll(namespace, beacon.metadata.name);
      setAsked(requestedAt);
      onPolled();
    } catch (e) {
      if (e instanceof Unauthenticated) setFailed("Your session has expired. Sign in again.");
      else if (e instanceof ApiError && e.status === 403)
        setFailed(`Not permitted: ${e.message}`);
      else setFailed(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  // The request landed when the controller echoes the token back. Until then it
  // is queued, and saying which of the two it is beats a spinner that stops.
  const handled = asked !== null && beacon.status?.lastHandledReconcileAt === asked;

  return (
    <section>
      <h2 className="text-sm font-medium">Check now</h2>
      <p className="mt-2 text-sm text-[var(--muted-foreground)]">
        Looks at the sources immediately instead of waiting for the next interval. Finds nothing
        if nothing has changed.
      </p>
      {beacon.spec.suspend && (
        <p className="mt-2 text-sm text-[var(--unknown)]">
          This Beacon is suspended. A check will be recorded but nothing will be emitted until it
          is resumed.
        </p>
      )}
      <button
        onClick={poll}
        disabled={busy}
        className="mt-3 flex items-center gap-1.5 rounded-md bg-[var(--primary)] px-3 py-1.5 text-sm font-medium text-[var(--primary-foreground)] disabled:opacity-50"
      >
        {busy ? (
          <Loader2 size={14} className="animate-spin" aria-hidden />
        ) : (
          <RefreshCw size={14} aria-hidden />
        )}
        Check for new versions
      </button>
      {asked && !failed && (
        <p className="mt-3 text-sm text-[var(--muted-foreground)]">
          {handled ? "Checked." : "Asked. The Beacon looks on its next turn."}
        </p>
      )}
      {failed && (
        <p role="alert" className="mt-3 text-sm text-[var(--destructive)]">
          {failed}
        </p>
      )}
    </section>
  );
}

function when(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
