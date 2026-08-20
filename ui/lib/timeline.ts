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

/**
 * took renders how long something between two timestamps took.
 *
 * Only for work that has finished. A step that is still running deliberately
 * has no duration here: the page does not yet refresh itself, so an elapsed
 * time computed once at render would freeze at whatever it was when the page
 * loaded — which reads as a stuck step rather than a stale number, and is worse
 * than saying nothing.
 */
export function took(startedAt?: string, finishedAt?: string): string | null {
  if (!startedAt || !finishedAt) return null;
  const ms = Date.parse(finishedAt) - Date.parse(startedAt);
  if (Number.isNaN(ms) || ms < 0) return null;

  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 === 0 ? `${m}m` : `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return m % 60 === 0 ? `${h}h` : `${h}h ${m % 60}m`;
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
