"use client";

import Link from "next/link";
import {
  CircleDot,
  Clock,
  DoorOpen,
  PauseCircle,
  TriangleAlert,
  XCircle,
} from "lucide-react";
import { api, type GateSummary, type Overview, type Totals } from "@/lib/api";
import { Panel, useLiveApi } from "@/components/loader";
import { HealthDot } from "@/components/health";
import { ActivityChart } from "@/components/activity";

/**
 * Everything, in one screen.
 *
 * Read in three passes, and built in that order: the cards answer "is anything
 * wrong?", the chart answers "has it been getting worse?", and the Gate cards
 * answer "where?". A board that opened with a table of forty Gates would make
 * every reader do all three themselves, every time.
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
                <Cards totals={o.totals} />

                <section>
                  <h2 className="text-sm font-medium">Crossings</h2>
                  <p className="mt-0.5 text-sm text-[var(--muted-foreground)]">
                    What entered a Gate, and what failed trying, over {o.activity.length} days.
                  </p>
                  <div className="mt-3 rounded-lg border border-[var(--border)] p-3">
                    <ActivityChart days={o.activity} />
                  </div>
                </section>

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
                    <div className="mt-3 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                      {n.gates.map((g) => (
                        <GateCard key={g.name} gate={g} namespace={n.namespace} />
                      ))}
                    </div>
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
 * The headline numbers.
 *
 * Only the cards that are not zero, apart from Gates itself. A row of noughts
 * is a row a reader checks every time to learn nothing, and it buries the one
 * number that is not zero among six that are.
 */
function Cards({ totals }: { totals: Totals }) {
  const cards = [
    { n: totals.gates, label: totals.gates === 1 ? "Gate" : "Gates", Icon: DoorOpen, tone: "", always: true },
    { n: totals.degraded, label: "degraded", Icon: TriangleAlert, tone: "text-[var(--destructive)]" },
    { n: totals.failed, label: totals.failed === 1 ? "failed crossing" : "failed crossings", Icon: XCircle, tone: "text-[var(--destructive)]" },
    { n: totals.suspended, label: "suspended", Icon: PauseCircle, tone: "text-[var(--unknown)]" },
    { n: totals.running, label: "crossing now", Icon: CircleDot, tone: "text-[var(--progressing)]" },
    { n: totals.eligible, label: "waiting to cross", Icon: Clock, tone: "text-[var(--muted-foreground)]" },
  ].filter((c) => c.always || c.n > 0);

  return (
    <section>
      {/* The all-clear is a sentence, not a card. "0 degraded" makes a reader
          check a number to learn nothing was wrong; saying so is one glance. */}
      {totals.degraded === 0 && totals.failed === 0 && totals.suspended === 0 && (
        <p className="mb-3 flex items-center gap-2 text-sm">
          <HealthDot health="Healthy" />
          <span className="text-[var(--muted-foreground)]">
            Nothing needs attention.
          </span>
        </p>
      )}

      <div className="grid gap-3 grid-cols-2 sm:grid-cols-3 lg:grid-cols-6">
        {cards.map(({ n, label, Icon, tone }) => (
          <div
            key={label}
            className="rounded-lg border border-[var(--border)] bg-[var(--card)] p-3"
          >
            <div className={`flex items-center gap-1.5 text-xs ${tone || "text-[var(--muted-foreground)]"}`}>
              <Icon size={13} aria-hidden />
              {label}
            </div>
            <div className={`mt-1 text-2xl font-semibold tabular-nums ${tone}`}>{n}</div>
          </div>
        ))}
      </div>
    </section>
  );
}

/** The colour of a Gate card's left edge: its health, at a glance. */
const edge: Record<string, string> = {
  Healthy: "border-l-[var(--healthy)]",
  Progressing: "border-l-[var(--progressing)]",
  Degraded: "border-l-[var(--destructive)]",
  Unknown: "border-l-[var(--unknown)]",
  NotApplicable: "border-l-[var(--border)]",
};

function GateCard({ gate, namespace }: { gate: GateSummary; namespace: string }) {
  return (
    <div
      className={`rounded-lg border border-l-4 border-[var(--border)] bg-[var(--card)] p-3 ${
        edge[gate.health] ?? edge.Unknown
      }`}
    >
      <div className="flex items-baseline gap-2">
        <Link
          href={{ pathname: "/gates/", query: { name: gate.name, namespace } }}
          className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
        >
          {gate.name}
        </Link>
        {/* The word as well as the colour, always: roughly one man in twelve
            cannot reliably tell the green from the red, and the edge stripe
            would be all this card said. */}
        <span className="ml-auto">
          <HealthDot health={gate.health} />
        </span>
      </div>

      <p className="mt-1 truncate text-sm text-[var(--muted-foreground)]" title={gate.current}>
        {gate.current || "nothing yet"}
      </p>

      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        {gate.suspended && (
          <span className="flex items-center gap-1 text-[var(--unknown)]">
            <PauseCircle size={12} aria-hidden />
            suspended
          </span>
        )}
        {gate.running && (
          <Link
            href={{ pathname: "/passages/", query: { name: gate.running, namespace } }}
            className="flex items-center gap-1 text-[var(--progressing)] underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
          >
            <CircleDot size={12} aria-hidden />
            crossing
          </Link>
        )}
        {gate.eligible > 0 && !gate.running && (
          <span className="flex items-center gap-1 text-[var(--muted-foreground)]">
            <Clock size={12} aria-hidden />
            {gate.eligible} waiting
          </span>
        )}
      </div>

      {/* The reason, not just the colour. A card showing a red edge and nothing
          else sends every reader to the same second page to find out why. */}
      {gate.issues?.length ? (
        <p className="mt-2 border-t border-[var(--border)] pt-2 text-xs text-[var(--muted-foreground)]">
          {gate.issues.join(" · ")}
        </p>
      ) : null}
    </div>
  );
}
