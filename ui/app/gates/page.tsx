"use client";

import { useState } from "react";
import Link from "next/link";
import { ArrowLeft, ArrowRight, CircleAlert, Loader2 } from "lucide-react";
import { api, ApiError, Unauthenticated, type Explanation } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { HealthDot } from "@/components/health";

/**
 * One Gate, and why nothing is crossing it.
 *
 * The Gate's name comes from a query parameter rather than a path segment. A
 * static export has to know every route at build time, and Gate names are not
 * knowable then — `/gates/?name=production` is uglier than `/gates/production`
 * and, unlike it, actually works and is still a link somebody can send.
 */
export default function GateDetail() {
  const ns = useNamespace();
  const name = useQueryParam("name", "");
  const [crossed, setCrossed] = useState(0);

  // `crossed` is in the deps so promoting reloads the explanation: the answer
  // to "why is nothing crossing" changes the moment something is.
  const state = useApi(() => api.explain(ns, name), [ns, name, crossed]);

  if (!name) {
    return (
      <p className="text-sm text-[var(--muted-foreground)]">
        No Gate named. <Link href="/" className="underline">Back to Gates</Link>.
      </p>
    );
  }

  return (
    <div>
      <Link
        href="/"
        className="mb-4 inline-flex items-center gap-1.5 text-sm text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
      >
        <ArrowLeft size={14} aria-hidden />
        Gates
      </Link>

      <Panel state={state}>
        {(ex: Explanation) => (
          <div className="space-y-8">
            <header>
              <div className="flex items-baseline gap-3">
                <h1 className="text-xl font-semibold tracking-tight">{ex.gate}</h1>
                <span className="text-sm text-[var(--muted-foreground)]">{ex.state}</span>
                <span className="ml-auto">
                  <HealthDot health={ex.health} />
                </span>
              </div>
              <p className="mt-1 text-sm text-[var(--muted-foreground)]">{ex.summary}</p>
              {ex.current && (
                <p className="mt-2 text-sm">
                  Running now: <span className="font-medium">{ex.current}</span>
                </p>
              )}
            </header>

            {ex.blockers && ex.blockers.length > 0 && (
              <section>
                <h2 className="text-sm font-medium">In the way</h2>
                <ul className="mt-2 space-y-3">
                  {ex.blockers.map((b, i) => (
                    <li key={`${b.kind}-${i}`} className="flex gap-3 text-sm">
                      <CircleAlert
                        size={16}
                        className="mt-0.5 shrink-0 text-[var(--unknown)]"
                        aria-hidden
                      />
                      <div>
                        <p>
                          <span className="font-mono text-xs text-[var(--muted-foreground)]">{b.kind}</span>{" "}
                          {b.detail}
                        </p>
                        {/* The fix is the whole point of an explanation: a
                            diagnosis nobody can act on is just a status. */}
                        {b.fix && (
                          <p className="mt-1 font-mono text-xs text-[var(--muted-foreground)]">→ {b.fix}</p>
                        )}
                      </div>
                    </li>
                  ))}
                </ul>
              </section>
            )}

            <Eligible
              namespace={ns}
              gate={ex.gate}
              bundles={ex.eligible ?? []}
              onCrossed={() => setCrossed((n) => n + 1)}
            />

            {ex.waiting && ex.waiting.length > 0 && (
              <section>
                <h2 className="text-sm font-medium">Waiting</h2>
                <ul className="mt-2 space-y-1.5 text-sm">
                  {ex.waiting.map((w) => (
                    <li key={w.bundle} className="flex gap-3">
                      <span className="font-medium">{w.bundle}</span>
                      <span className="text-[var(--muted-foreground)]">{w.reason}</span>
                    </li>
                  ))}
                </ul>
              </section>
            )}
          </div>
        )}
      </Panel>
    </div>
  );
}

function Eligible({
  namespace,
  gate,
  bundles,
  onCrossed,
}: {
  namespace: string;
  gate: string;
  bundles: string[];
  onCrossed: () => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const [failed, setFailed] = useState<string | null>(null);

  if (bundles.length === 0) {
    return (
      <section>
        <h2 className="text-sm font-medium">Eligible</h2>
        <p className="mt-2 text-sm text-[var(--muted-foreground)]">Nothing may cross right now.</p>
      </section>
    );
  }

  async function cross(bundle: string) {
    setBusy(bundle);
    setFailed(null);
    try {
      await api.promote(namespace, gate, bundle);
      onCrossed();
    } catch (e) {
      // Refusals are reported in full rather than as "failed". The API
      // distinguishes "you may not" from "the rules say no", and both are
      // answers the person clicking needs to read.
      if (e instanceof Unauthenticated) setFailed("Your session has expired. Sign in again.");
      else if (e instanceof ApiError && e.status === 403)
        setFailed(`Not permitted: ${e.message}`);
      else setFailed(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <section>
      <h2 className="text-sm font-medium">Eligible</h2>
      <ul className="mt-2 space-y-2">
        {bundles.map((b) => (
          <li key={b} className="flex items-center gap-3 text-sm">
            <span className="font-medium">{b}</span>
            <button
              onClick={() => cross(b)}
              disabled={busy !== null}
              className="ml-auto flex items-center gap-1.5 rounded-md bg-[var(--primary)] px-3 py-1.5 text-sm font-medium text-[var(--primary-foreground)] disabled:opacity-50"
            >
              {busy === b ? (
                <Loader2 size={14} className="animate-spin" aria-hidden />
              ) : (
                <ArrowRight size={14} aria-hidden />
              )}
              Cross {gate}
            </button>
          </li>
        ))}
      </ul>
      {failed && (
        <p role="alert" className="mt-3 text-sm text-[var(--destructive)]">
          {failed}
        </p>
      )}
    </section>
  );
}
