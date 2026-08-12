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
