"use client";

import { useEffect, useState } from "react";
import { LogIn, TriangleAlert } from "lucide-react";
import { ApiError, Unauthenticated, signIn } from "@/lib/api";
import { useQueryParam } from "@/lib/browser";

/** useNamespace reads the namespace from the URL, defaulting to `default`. */
export function useNamespace(): string {
  return useQueryParam("namespace", "default");
}

type State<T> = { data?: T; error?: unknown; loading: boolean };

/**
 * useApi loads from the API and reports the three states a page has to render.
 *
 * The unauthenticated case is separated from every other error on purpose: it
 * is the one where the user can do something, and showing "401" to someone
 * whose session simply expired is a dead end.
 */
export function useApi<T>(load: () => Promise<T>, deps: unknown[]): State<T> {
  const [state, setState] = useState<State<T>>({ loading: true });

  useEffect(() => {
    let live = true;
    // No synchronous setState({loading:true}) here: React 19 flags it, and it
    // is not wanted anyway. Changing namespace keeps the previous data on
    // screen until the new data arrives, which reads better than a flash of
    // "Loading…" between two populated tables.
    load()
      .then((data) => live && setState({ data, loading: false }))
      .catch((error) => live && setState({ error, loading: false }));
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return state;
}

/** Panel renders loading, the sign-in prompt, an error, or the children. */
export function Panel<T>({
  state,
  children,
}: {
  state: State<T>;
  children: (data: T) => React.ReactNode;
}) {
  if (state.loading) {
    return <p className="text-sm text-[var(--muted)]">Loading…</p>;
  }

  if (state.error instanceof Unauthenticated) {
    return (
      <div className="rounded-lg border border-[var(--line)] p-6">
        <h2 className="font-medium">Sign in</h2>
        <p className="mt-1 text-sm text-[var(--muted)]">
          Hecate asks Kubernetes who you are and what you may do. Signing in gets a token the
          cluster already trusts — it grants nothing on its own.
        </p>
        <button
          onClick={signIn}
          className="mt-4 flex items-center gap-2 rounded-md bg-[var(--color-accent)] px-3 py-1.5 text-sm font-medium text-white"
        >
          <LogIn size={15} aria-hidden />
          Sign in
        </button>
      </div>
    );
  }

  if (state.error) {
    const err = state.error;
    const forbidden = err instanceof ApiError && err.status === 403;
    return (
      <div className="flex items-start gap-3 rounded-lg border border-[var(--line)] p-6">
        <TriangleAlert size={18} className="mt-0.5 text-[var(--color-degraded)]" aria-hidden />
        <div>
          <h2 className="font-medium">{forbidden ? "Not permitted" : "Could not load"}</h2>
          <p className="mt-1 text-sm text-[var(--muted)]">
            {err instanceof Error ? err.message : String(err)}
          </p>
          {forbidden && (
            <p className="mt-2 text-sm text-[var(--muted)]">
              Your Kubernetes user may read some namespaces and not others. Hecate does not
              decide this; the cluster does.
            </p>
          )}
        </div>
      </div>
    );
  }

  return <>{children(state.data as T)}</>;
}
