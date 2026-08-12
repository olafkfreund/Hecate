"use client";

import { CircleCheck, CircleX, Sparkles } from "lucide-react";
import { timeline, type TimelineEvent } from "@/lib/timeline";
import type { Bundle } from "@/lib/api";

const marks = {
  emitted: { Icon: Sparkles, tone: "text-[var(--muted)]", what: "emitted" },
  cleared: { Icon: CircleCheck, tone: "text-[var(--color-healthy)]", what: "cleared" },
  blocked: { Icon: CircleX, tone: "text-[var(--color-degraded)]", what: "did not clear" },
} as const;

/** How this Bundle got to where it is. */
export function Timeline({ bundle }: { bundle: Bundle }) {
  const events = timeline(bundle);

  if (events.length === 0) {
    return <p className="text-sm text-[var(--muted)]">Nothing has happened to this Bundle yet.</p>;
  }

  return (
    <ol className="relative space-y-5 border-l border-[var(--line)] pl-6">
      {events.map((e, i) => (
        <Event key={`${e.kind}-${e.gate ?? "emitted"}-${i}`} event={e} />
      ))}
    </ol>
  );
}

function Event({ event }: { event: TimelineEvent }) {
  const { Icon, tone, what } = marks[event.kind];

  return (
    <li className="relative">
      <span className={`absolute -left-[31px] bg-[var(--bg)] p-0.5 ${tone}`}>
        <Icon size={15} aria-hidden />
      </span>

      <p className="text-sm">
        <span className="font-medium">{event.gate ? `${what} ${event.gate}` : "emitted"}</span>
        {/* The actor is the point of an audit trail. "controller" is a real
            answer — it says nobody asked, the Gate crosses automatically. */}
        {event.actor && <span className="text-[var(--muted)]"> by {event.actor}</span>}
      </p>

      {event.reason && <p className="mt-0.5 text-sm text-[var(--muted)]">{event.reason}</p>}

      <p className="mt-0.5 text-xs text-[var(--muted)]">
        <time dateTime={event.at}>{when(event.at)}</time>
        {event.passage && <span className="ml-2 font-mono">{event.passage}</span>}
      </p>
    </li>
  );
}

/**
 * when renders a timestamp readably.
 *
 * The absolute time, not "3 hours ago": this is an audit trail, and the
 * question it answers is "when did this reach production", which a relative
 * time cannot be compared against a change record or an incident.
 */
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
