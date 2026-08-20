"use client";

import Link from "next/link";
import { CircleDot, PauseCircle, TriangleAlert } from "lucide-react";
import { api, type GateSummary, type Overview, type Totals } from "@/lib/api";
import { Panel, useLiveApi } from "@/components/loader";
import { HealthDot } from "@/components/health";

/**
 * Everything, in one screen.
 *
 * The first thing on the page answers "is anything wrong?", because that is the
 * question someone opening a dashboard is actually asking; the Gates below
 * answer "what, and where"; and the pages they link to answer the rest. A board
 * that opened with a table of forty Gates would make every reader do the
 * summarising themselves, every time.
 *
 * Cluster-wide, and deliberately not namespaced — the namespace picker does not
 * apply here, which is the point of it existing.
 */
export default function OverviewPage() {
  const state = useLiveApi(() => api.overview(), []);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Overview</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        Every Gate you can see, wherever it is.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(o: Overview) =>
            o.totals.gates === 0 ? (
              <p className="text-sm text-[var(--muted-foreground)]">
                No Gates anywhere you can read. Either none exist yet, or your Kubernetes user
                cannot see the namespaces holding them — Hecate does not decide this; the cluster
                does.
              </p>
            ) : (
              <div className="space-y-8">
                <Summary totals={o.totals} />
                {o.namespaces.map((n) => (
                  <section key={n.namespace}>
                    <h2 className="text-sm font-medium">
                      <Link
                        href={{ pathname: "/gates/", query: { namespace: n.namespace } }}
                        className="underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                      >
                        {n.namespace}
                      </Link>
                    </h2>
                    <ul className="mt-2 divide-y divide-[var(--border)]">
                      {n.gates.map((g) => (
                        <GateRow key={g.name} gate={g} namespace={n.namespace} />
                      ))}
                    </ul>
                  </section>
                ))}
              </div>
            )
          }
        </Panel>
      </div>
    </div>
  );
}

/**
 * Summary is the headline: what needs attention, then what is happening.
 *
 * Only the counts that are not zero, apart from Gates itself. A row of zeroes
 * is a row a reader has to check every time to learn nothing, and it buries the
 * one number that is not zero among six that are.
 */
function Summary({ totals }: { totals: Totals }) {
  const attention = [
    { n: totals.degraded, label: totals.degraded === 1 ? "degraded" : "degraded", tone: "text-[var(--destructive)]" },
    { n: totals.failed, label: totals.failed === 1 ? "failed crossing" : "failed crossings", tone: "text-[var(--destructive)]" },
    { n: totals.suspended, label: "suspended", tone: "text-[var(--unknown)]" },
  ].filter((c) => c.n > 0);

  const activity = [
    { n: totals.running, label: totals.running === 1 ? "crossing now" : "crossing now" },
    { n: totals.eligible, label: totals.eligible === 1 ? "waiting to cross" : "waiting to cross" },
    { n: totals.progressing, label: "progressing" },
  ].filter((c) => c.n > 0);

  return (
    <section className="rounded-lg border border-[var(--border)] p-4">
      {attention.length === 0 ? (
        <p className="flex items-center gap-2 text-sm">
          <HealthDot health="Healthy" />
          <span className="text-[var(--muted-foreground)]">
            {totals.gates === 1 ? "1 Gate" : `All ${totals.gates} Gates`} healthy.
          </span>
        </p>
      ) : (
        <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
          <TriangleAlert size={16} className="text-[var(--destructive)]" aria-hidden />
          {attention.map((c) => (
            <span key={c.label} className={c.tone}>
              <span className="font-semibold tabular-nums">{c.n}</span> {c.label}
            </span>
          ))}
          <span className="text-[var(--muted-foreground)]">of {totals.gates}</span>
        </p>
      )}

      {activity.length > 0 && (
        <p className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-[var(--muted-foreground)]">
          {activity.map((c) => (
            <span key={c.label}>
              <span className="font-semibold tabular-nums">{c.n}</span> {c.label}
            </span>
          ))}
        </p>
      )}
    </section>
  );
}

function GateRow({ gate, namespace }: { gate: GateSummary; namespace: string }) {
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-1 py-2.5 text-sm">
      <Link
        href={{ pathname: "/gates/", query: { name: gate.name, namespace } }}
        className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
      >
        {gate.name}
      </Link>

      <span className="text-[var(--muted-foreground)]">{gate.current || "nothing yet"}</span>

      {/* A suspended Gate is usually healthy and admitting nothing, which is the
          one state a health dot alone describes wrongly — and "nothing has
          shipped all week" is usually this. */}
      {gate.suspended && (
        <span className="flex items-center gap-1 text-[var(--unknown)]">
          <PauseCircle size={13} aria-hidden />
          suspended
        </span>
      )}

      {gate.running && (
        <Link
          href={{ pathname: "/passages/", query: { name: gate.running, namespace } }}
          className="flex items-center gap-1 text-[var(--progressing)] underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
        >
          <CircleDot size={13} aria-hidden />
          crossing
        </Link>
      )}

      {gate.eligible > 0 && !gate.running && (
        <span className="text-[var(--muted-foreground)]">
          {gate.eligible} waiting
        </span>
      )}

      <span className="ml-auto">
        <HealthDot health={gate.health} />
      </span>

      {/* The reason, not just the colour. A board showing a red dot and nothing
          else sends every reader to the same second page to find out why. */}
      {gate.issues?.length ? (
        <p className="w-full text-[var(--muted-foreground)]">{gate.issues.join(" · ")}</p>
      ) : null}
    </li>
  );
}
