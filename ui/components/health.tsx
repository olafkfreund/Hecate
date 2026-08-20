import type { Health } from "@/lib/api";

const tone: Record<Health, string> = {
  Healthy: "text-[var(--healthy)]",
  Progressing: "text-[var(--progressing)]",
  Degraded: "text-[var(--destructive)]",
  Unknown: "text-[var(--unknown)]",
  NotApplicable: "text-[var(--muted-foreground)]",
};

/**
 * HealthDot shows a Gate's health.
 *
 * The word is always present, never colour alone: roughly one man in twelve
 * cannot reliably tell the green from the red, and health is the one thing on
 * this page nobody should have to guess at.
 */
export function HealthDot({ health }: { health?: Health }) {
  const h = health ?? "Unknown";
  return (
    <span className={`inline-flex items-center gap-1.5 text-sm ${tone[h]}`}>
      <span aria-hidden className="size-2 rounded-full bg-current" />
      {h}
    </span>
  );
}

/**
 * The CSS variable each health maps to.
 *
 * One map, because health colour now appears in four places — the dot, the
 * pipeline graph's node, a card's edge, and the Overview's cards — and four
 * copies of it is four chances for a Degraded Gate to be red in one view and
 * amber in another.
 */
export const healthVar: Record<Health, string> = {
  Healthy: "var(--healthy)",
  Progressing: "var(--progressing)",
  Degraded: "var(--destructive)",
  Unknown: "var(--unknown)",
  // Not a colour of its own: "this Gate has no health to report" is not a
  // state anyone needs to pick out of a row, and giving it one would put a
  // fifth hue on screen that means nothing.
  NotApplicable: "var(--border)",
};

/**
 * The left-edge stripe class for a card.
 *
 * Written out rather than built from healthVar with a template literal, which
 * is the obvious spelling and silently produces nothing: Tailwind generates
 * arbitrary values by scanning the source for the literal class name, so a
 * class assembled at runtime is one the stylesheet never contains. The card
 * would render with no stripe at all and nothing would fail.
 */
const edge: Record<Health, string> = {
  Healthy: "border-l-[var(--healthy)]",
  Progressing: "border-l-[var(--progressing)]",
  Degraded: "border-l-[var(--destructive)]",
  Unknown: "border-l-[var(--unknown)]",
  NotApplicable: "border-l-[var(--border)]",
};

export function healthEdge(health?: Health): string {
  return edge[health ?? "Unknown"];
}
