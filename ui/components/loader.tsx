"use client";

import { useEffect, useState } from "react";
import { LogIn, TriangleAlert } from "lucide-react";
import { ApiError, Unauthenticated, signIn } from "@/lib/api";
import { useQueryParam } from "@/lib/browser";
import { useLive } from "@/lib/live";

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

/**
 * useLiveApi is useApi that reloads when the server says the namespace moved.
 *
 * Separate from useApi rather than a flag on it, because the two answer to
 * different things: useApi reloads when its own inputs change, and a page using
 * it is correct as of when it loaded. useLiveApi additionally follows the
 * cluster, and is the right choice for anything showing state that moves on its
 * own — a Passage running its steps, a Gate whose health is being rechecked.
 *
 * A page using this still works with no stream: useLive stays at zero, the
 * dependency never changes, and the behaviour is exactly what it was before.
 */
export function useLiveApi<T>(load: () => Promise<T>, deps: unknown[]): State<T> {
  const changes = useLive(useNamespace());
  return useApi(load, [...deps, changes]);
}

/**
 * useLiveAll is useLiveApi for a page that spans every namespace.
 *
 * Which is most of them: the list pages show the whole cluster the viewer can
 * read, so watching one namespace would leave them stale whenever anything
 * moved anywhere else — the failure being invisible, because a page that
 * quietly stops updating looks exactly like a page where nothing happened.
 */
export function useLiveAll<T>(load: () => Promise<T>, deps: unknown[]): State<T> {
  const changes = useLive();
  return useApi(load, [...deps, changes]);
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
    return <p className="text-sm text-[var(--muted-foreground)]">Loading…</p>;
  }

  if (state.error instanceof Unauthenticated) {
    return (
      <div className="rounded-lg border border-[var(--border)] p-6">
        <h2 className="font-medium">Sign in</h2>
        <p className="mt-1 text-sm text-[var(--muted-foreground)]">
          Hecate asks Kubernetes who you are and what you may do. Signing in gets a token the
          cluster already trusts — it grants nothing on its own.
        </p>
        <button
          onClick={signIn}
          className="mt-4 flex items-center gap-2 rounded-md bg-[var(--primary)] px-3 py-1.5 text-sm font-medium text-[var(--primary-foreground)]"
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
      <div className="flex items-start gap-3 rounded-lg border border-[var(--border)] p-6">
        <TriangleAlert size={18} className="mt-0.5 text-[var(--destructive)]" aria-hidden />
        <div>
          <h2 className="font-medium">{forbidden ? "Not permitted" : "Could not load"}</h2>
          <p className="mt-1 text-sm text-[var(--muted-foreground)]">
            {err instanceof Error ? err.message : String(err)}
          </p>
          {forbidden && (
            <p className="mt-2 text-sm text-[var(--muted-foreground)]">
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
