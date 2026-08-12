"use client";

import { useSyncExternalStore } from "react";

/**
 * useHydrated reports whether React has taken over from the prerendered HTML.
 *
 * The obvious spelling is `useState(false)` plus `setMounted(true)` in an
 * effect, which React 19 now flags: a synchronous setState in an effect
 * triggers a second render pass for every component that does it.
 * useSyncExternalStore says the same thing in one render — the server snapshot
 * is false, the client snapshot is true, and React reconciles the difference
 * without a cascade.
 */
export function useHydrated(): boolean {
  return useSyncExternalStore(
    () => () => {},
    () => true,
    () => false,
  );
}

/**
 * useQueryParam reads a query parameter, re-reading it when history changes.
 *
 * Not useSearchParams: a statically exported page has no server-side params,
 * and Next requires a Suspense boundary around it that would exist only to
 * satisfy the framework. Not an effect either, for the reason above.
 */
export function useQueryParam(name: string, fallback: string): string {
  return useSyncExternalStore(
    (onChange) => {
      window.addEventListener("popstate", onChange);
      return () => window.removeEventListener("popstate", onChange);
    },
    () => new URLSearchParams(window.location.search).get(name) ?? fallback,
    () => fallback,
  );
}
