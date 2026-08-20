"use client";

import { useEffect, useState } from "react";

/**
 * useLive reports how many times the server has said this namespace changed.
 *
 * The number is meaningless on its own — it exists to go in a useApi dependency
 * list, so a page reloads through the endpoint it already uses. The stream
 * carries no resources for that reason: authorisation is decided by the guarded
 * handlers, once, rather than again here per event, and someone whose access is
 * revoked gets a 403 on the refetch instead of data over the stream.
 *
 * A stream that cannot be opened leaves the count at zero for ever, which is
 * exactly the behaviour every page had before this existed. Live updates are an
 * improvement on a working page, never a requirement for one.
 */
export function useLive(namespace: string): number {
  const [changes, setChanges] = useState(0);

  useEffect(() => {
    // EventSource cannot set headers, so this rides the session cookie the
    // browser attaches on its own — which is the same credential the API reads
    // from the Authorization header, arriving by the other route it supports.
    const url = `/api/v1alpha1/namespaces/${encodeURIComponent(namespace)}/watch`;

    let source: EventSource;
    try {
      // No `typeof EventSource === "undefined"` guard before this: constructing
      // an undefined global throws a ReferenceError that lands here, so the
      // guard was a second spelling of the same check and only the catch is
      // load-bearing. A browser without EventSource, and a browser that refuses
      // the connection, are the same case and want the same answer.
      source = new EventSource(url, { withCredentials: true });
    } catch {
      return;
    }

    const bump = () => setChanges((n) => n + 1);
    source.addEventListener("changed", bump);

    // Deliberately quiet on error. EventSource reconnects on its own, and a
    // banner saying "live updates unavailable" over a page that is showing
    // correct data would report a degraded luxury as a fault.
    source.onerror = () => {};

    return () => {
      source.removeEventListener("changed", bump);
      source.close();
    };
  }, [namespace]);

  return changes;
}
