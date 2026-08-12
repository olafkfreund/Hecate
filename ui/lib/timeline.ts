import type { Bundle, GateCrossing } from "@/lib/api";

export type EventKind = "emitted" | "cleared" | "blocked";

export interface TimelineEvent {
  kind: EventKind;
  at: string;
  gate?: string;
  actor?: string;
  reason?: string;
  passage?: string;
}

/**
 * One Bundle's history, oldest first.
 *
 * Built from `status.cleared` and `status.blocked` rather than by correlating
 * Passages: the Bundle already records the gate, the actor and the time of
 * every outcome, and Passages are collected under retention (D40) — so a
 * timeline built from them would lose its own past.
 *
 * The emission is included as the first event because "when did this appear"
 * is the start of the story, and it is the one moment no crossing records.
 */
export function timeline(bundle: Bundle): TimelineEvent[] {
  const events: TimelineEvent[] = [];

  if (bundle.metadata.creationTimestamp) {
    events.push({ kind: "emitted", at: bundle.metadata.creationTimestamp });
  }
  for (const c of bundle.status?.cleared ?? []) {
    events.push({ kind: "cleared", ...pick(c) });
  }
  for (const b of bundle.status?.blocked ?? []) {
    events.push({ kind: "blocked", ...pick(b) });
  }

  // Oldest first: this is a history, and a history read backwards makes the
  // consequence arrive before the cause.
  return events.sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
}

function pick(c: GateCrossing) {
  return { at: c.at, gate: c.gate, actor: c.actor, reason: c.reason, passage: c.passage };
}

/** describeArtifact is what the artifact is, in one line. */
export function describeArtifact(a: {
  image?: { repo: string; tag?: string; digest?: string };
  chart?: { repo?: string; name?: string; version?: string };
  commit?: { repo?: string; sha?: string; ref?: string };
}): { what: string; detail?: string } {
  if (a.image) {
    return {
      what: a.image.tag ? `${a.image.repo}:${a.image.tag}` : a.image.repo,
      // The digest, not the tag, is what was actually deployed.
      detail: a.image.digest,
    };
  }
  if (a.chart) {
    return { what: `${a.chart.name ?? "chart"} ${a.chart.version ?? ""}`.trim(), detail: a.chart.repo };
  }
  if (a.commit) {
    return { what: a.commit.ref ?? a.commit.sha ?? "commit", detail: a.commit.sha };
  }
  return { what: "unknown artifact" };
}
