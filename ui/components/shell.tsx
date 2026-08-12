"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useTheme } from "next-themes";
import { useEffect, useState } from "react";
import { Activity, DoorOpen, Package, Route, Moon, Sun, Monitor } from "lucide-react";
import { useHydrated, useQueryParam } from "@/lib/browser";

const nav = [
  { href: "/", label: "Gates", icon: DoorOpen },
  { href: "/bundles/", label: "Bundles", icon: Package },
  { href: "/passages/", label: "Passages", icon: Route },
];

export function Shell({ children }: { children: React.ReactNode }) {
  const path = usePathname();

  return (
    <div className="mx-auto flex min-h-screen max-w-6xl flex-col px-6">
      <header className="flex items-center gap-6 border-b border-[var(--line)] py-4">
        <Link href="/" className="flex items-center gap-2 font-semibold tracking-tight">
          <span aria-hidden className="text-lg">
            ⛿
          </span>
          Hecate
        </Link>

        <nav className="flex items-center gap-1" aria-label="Main">
          {nav.map(({ href, label, icon: Icon }) => {
            // The Gates tab is "/", which every path starts with, so it only
            // matches exactly. Without this every tab looks active at once.
            const active = href === "/" ? path === "/" : path.startsWith(href);
            return (
              <Link
                key={href}
                href={href}
                aria-current={active ? "page" : undefined}
                className={`flex items-center gap-2 rounded-md px-3 py-1.5 text-sm transition-colors ${
                  active
                    ? "bg-[var(--raised)] font-medium"
                    : "text-[var(--muted)] hover:bg-[var(--raised)]"
                }`}
              >
                <Icon size={15} aria-hidden />
                {label}
              </Link>
            );
          })}
        </nav>

        <div className="ml-auto flex items-center gap-3">
          <Namespace />
          <ThemeToggle />
        </div>
      </header>

      <main className="flex-1 py-8">{children}</main>

      <footer className="border-t border-[var(--line)] py-4 text-xs text-[var(--muted)]">
        <Version />
      </footer>
    </div>
  );
}

/**
 * Namespace is where you are looking.
 *
 * Kept in the URL query rather than in component state so a link to a Gate in
 * one namespace is a link someone else can open — a dashboard whose address bar
 * does not describe what is on screen cannot be shared, which is most of what
 * people do with one.
 */
function Namespace() {
  const current = useQueryParam("namespace", "default");
  // `edited` is null until the field is touched, so the box follows the URL —
  // it used to seed useState from the *hydration-time* snapshot, which is
  // always the fallback, so it read "default" while the page was showing
  // another namespace entirely.
  const [edited, setEdited] = useState<string | null>(null);
  const ns = edited ?? current;

  return (
    <label className="flex items-center gap-2 text-sm text-[var(--muted)]">
      <span className="sr-only">Namespace</span>
      <input
        value={ns}
        onChange={(e) => setEdited(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") e.currentTarget.blur();
        }}
        onBlur={() => {
          if (edited === null || edited === current) return;
          const url = new URL(window.location.href);
          url.searchParams.set("namespace", edited);
          window.location.href = url.toString();
        }}
        className="w-32 rounded-md border border-[var(--line)] bg-transparent px-2 py-1 text-[var(--fg)]"
        aria-label="Namespace"
      />
    </label>
  );
}

function ThemeToggle() {
  const { theme, setTheme } = useTheme();

  // The prerendered HTML cannot know the theme, so rendering the real control
  // before hydration would mismatch and React would complain.
  if (!useHydrated()) return <div className="size-8" aria-hidden />;

  const next = theme === "dark" ? "light" : theme === "light" ? "system" : "dark";
  const Icon = theme === "dark" ? Moon : theme === "light" ? Sun : Monitor;

  return (
    <button
      onClick={() => setTheme(next)}
      className="rounded-md p-2 text-[var(--muted)] hover:bg-[var(--raised)]"
      aria-label={`Theme: ${theme ?? "system"}. Switch to ${next}.`}
      title={`Theme: ${theme ?? "system"}`}
    >
      <Icon size={16} aria-hidden />
    </button>
  );
}

function Version() {
  const [version, setVersion] = useState<string | null>(null);

  useEffect(() => {
    // /healthz is unauthenticated, so this works on the sign-in screen too and
    // tells you which build is answering before you can see anything else.
    fetch("/healthz")
      .then((r) => (r.ok ? r.json() : null))
      .then((b) => setVersion(b?.version ?? null))
      .catch(() => setVersion(null));
  }, []);

  return (
    <span className="flex items-center gap-2">
      <Activity size={13} aria-hidden />
      {version ? `hecate ${version}` : "hecate"}
    </span>
  );
}
