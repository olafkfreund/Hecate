"use client";

import { useEffect } from "react";
import { TriangleAlert } from "lucide-react";

/**
 * What a crash looks like, instead of Next's blank "Application error".
 *
 * That default says a client-side exception occurred and directs you to the
 * console, which is no help at all to whoever is looking at the screen — and it
 * loses the message even for someone who does open the console after the fact.
 * This keeps the error on the page, where the person who hit it can read it and
 * send it on.
 */
export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Also to the console, so it is in the place a developer looks first.
    console.error("hecate: unhandled error", error);
  }, [error]);

  return (
    <div className="flex items-start gap-3 rounded-lg border border-[var(--border)] p-6">
      <TriangleAlert size={18} className="mt-0.5 text-[var(--destructive)]" aria-hidden />
      <div className="min-w-0">
        <h1 className="font-medium">Something in the page went wrong</h1>
        <p className="mt-1 text-sm text-[var(--muted-foreground)]">
          This is a bug in Hecate&apos;s UI, not in your cluster. Nothing was changed.
        </p>
        <pre className="mt-3 max-w-full overflow-x-auto rounded-md bg-[var(--secondary)] p-3 text-xs">
          {error.message}
          {error.digest ? `\n\ndigest: ${error.digest}` : ""}
        </pre>
        <div className="mt-4 flex gap-2">
          <button
            onClick={reset}
            className="rounded-md bg-[var(--primary)] px-3 py-1.5 text-sm font-medium text-[var(--primary-foreground)]"
          >
            Try again
          </button>
          <button
            onClick={() => window.location.reload()}
            className="rounded-md border border-[var(--border)] px-3 py-1.5 text-sm"
          >
            Reload
          </button>
        </div>
        <p className="mt-3 text-xs text-[var(--muted-foreground)]">
          If this appeared after an upgrade, a full reload is usually enough: the page may
          be holding assets from the previous build.
        </p>
      </div>
    </div>
  );
}
