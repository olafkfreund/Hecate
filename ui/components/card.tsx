"use client";

import Link from "next/link";
import type { ReactNode } from "react";

/**
 * The list primitives.
 *
 * Every list page grew its own row of muted text, and between them they showed
 * about a third of what the API returns — the fields that were easy to put in a
 * sentence, rather than the ones someone opens the page to find. These exist so
 * a page can be rewritten as "what does a reader need" instead of "what fits on
 * one line", and so that a Degraded thing looks the same on every screen.
 */

/** Card is one item in a list: a heading row, then whatever the page adds. */
export function Card({
  href,
  title,
  subtitle,
  edge,
  right,
  children,
}: {
  href?: { pathname: string; query: Record<string, string> };
  title: string;
  subtitle?: string;
  /** A Tailwind border-l-[...] class. Colour is never the only signal — the
   *  caller is expected to put the same state in words in `right`. */
  edge?: string;
  right?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div
      className={`rounded-lg border border-[var(--border)] bg-[var(--card)] p-3 ${
        edge ? `border-l-4 ${edge}` : ""
      }`}
    >
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        {href ? (
          <Link
            href={href}
            className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
          >
            {title}
          </Link>
        ) : (
          <span className="font-medium">{title}</span>
        )}
        {subtitle && (
          <span className="truncate text-sm text-[var(--muted-foreground)]" title={subtitle}>
            {subtitle}
          </span>
        )}
        {right && <span className="ml-auto shrink-0">{right}</span>}
      </div>
      {children}
    </div>
  );
}

/** Meta is the row of small facts under a card's heading. */
export function Meta({ children }: { children: ReactNode }) {
  return <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">{children}</div>;
}

const tones = {
  good: "text-[var(--healthy)]",
  bad: "text-[var(--destructive)]",
  busy: "text-[var(--progressing)]",
  warn: "text-[var(--unknown)]",
  quiet: "text-[var(--muted-foreground)]",
} as const;

/**
 * Pill is one fact, with an icon and a word.
 *
 * Always a word, never an icon or a colour alone: roughly one man in twelve
 * cannot reliably tell the green from the red, and a row of bare icons is a
 * legend someone has to learn before the page means anything.
 */
export function Pill({
  icon: Icon,
  tone = "quiet",
  children,
  href,
}: {
  icon?: React.ComponentType<{ size?: number | string; "aria-hidden"?: boolean }>;
  tone?: keyof typeof tones;
  children: ReactNode;
  href?: { pathname: string; query: Record<string, string> };
}) {
  const body = (
    <>
      {Icon && <Icon size={12} aria-hidden />}
      {children}
    </>
  );
  const cls = `flex items-center gap-1 ${tones[tone]}`;
  return href ? (
    <Link
      href={href}
      className={`${cls} underline decoration-[var(--border)] underline-offset-4 hover:decoration-current`}
    >
      {body}
    </Link>
  ) : (
    <span className={cls}>{body}</span>
  );
}

/** Note is a card's footnote — a reason, a message, an error. */
export function Note({ tone = "quiet", children }: { tone?: keyof typeof tones; children: ReactNode }) {
  return (
    <p className={`mt-2 border-t border-[var(--border)] pt-2 text-xs break-words ${tones[tone]}`}>
      {children}
    </p>
  );
}

/** Grid lays cards out: one column on a phone, more as there is room. */
export function Grid({ children }: { children: ReactNode }) {
  return <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">{children}</div>;
}

/** Rows is the single-column form, for cards that carry a lot of text. */
export function Rows({ children }: { children: ReactNode }) {
  return <div className="flex flex-col gap-3">{children}</div>;
}

/**
 * ago renders how long since a moment, for things that are still true.
 *
 * "4m ago" rather than a timestamp, because these answer "is this current?" —
 * a question about distance from now, not about when. The audit trail uses the
 * absolute time instead, and deliberately so: it answers "when did this reach
 * production", which has to line up against a change record.
 */
export function ago(iso?: string): string | null {
  if (!iso) return null;
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return null;
  const s = Math.round((Date.now() - then) / 1000);
  if (s < 0) return null;
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  return `${d}d ago`;
}
