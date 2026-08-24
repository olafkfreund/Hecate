"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useTheme } from "next-themes";
import { useEffect, useState } from "react";
import { Activity, DoorOpen, LayoutDashboard, Package, Radio, Route, ShieldCheck, Moon, Sun, Monitor, Cog, ScrollText } from "lucide-react";
import { useHydrated } from "@/lib/browser";

// Ordered the way a Bundle travels: a Beacon sees a version, emits a Bundle, a
// Gate admits it, a Passage carries it. Someone learning the model reads the
// nav left to right and gets the pipeline in the right order.
const nav = [
  { href: "/", label: "Overview", icon: LayoutDashboard },
  { href: "/gates/", label: "Gates", icon: DoorOpen },
  { href: "/beacons/", label: "Beacons", icon: Radio },
  { href: "/bundles/", label: "Bundles", icon: Package },
  { href: "/passages/", label: "Passages", icon: Route },
  { href: "/approvals/", label: "Approvals", icon: ShieldCheck },
  { href: "/audit/", label: "Audit", icon: ScrollText },
  { href: "/settings/", label: "Settings", icon: Cog },
];

export function Shell({ children }: { children: React.ReactNode }) {
  const path = usePathname();

  return (
    <div className="mx-auto flex min-h-screen max-w-6xl flex-col px-6">
      <header className="flex items-center gap-6 border-b border-[var(--border)] py-4">
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
                    ? "bg-[var(--secondary)] font-medium"
                    : "text-[var(--muted-foreground)] hover:bg-[var(--secondary)]"
                }`}
              >
                <Icon size={15} aria-hidden />
                {label}
              </Link>
            );
          })}
        </nav>

        {/* No namespace picker. Every page shows every namespace the viewer
            can read, grouped — see NamespaceGroups. A picker made "what is
            happening" a question you could only ask about a place you had
            already guessed, and it hid the answer everywhere else. */}
        <div className="ml-auto flex items-center gap-3">
          <ThemeToggle />
        </div>
      </header>

      <main className="flex-1 py-8">{children}</main>

      <footer className="border-t border-[var(--border)] py-4 text-xs text-[var(--muted-foreground)]">
        <Version />
      </footer>
    </div>
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
      className="rounded-md p-2 text-[var(--muted-foreground)] hover:bg-[var(--secondary)]"
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
