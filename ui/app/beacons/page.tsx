"use client";

import Link from "next/link";
import { api, type Beacon, type WatchSource } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { BeaconDetail } from "./detail";

export default function Beacons() {
  const ns = useNamespace();
  const selected = useQueryParam("name", "");
  if (selected) return <BeaconDetail />;
  return <BeaconList ns={ns} />;
}

/**
 * describeWatch names a source the way its own ecosystem does.
 *
 * A Beacon may watch several sources of different kinds, and which kind it is
 * decides what identifies it — flattening them all to "repo" would make a chart
 * and the image it deploys look like the same thing.
 */
export function describeWatch(w: WatchSource): string {
  if (w.image) return w.image.repo + (w.image.constraint ? ` ${w.image.constraint}` : "");
  if (w.chart) {
    const name = w.chart.name ? `${w.chart.name} ` : "";
    return `${name}${w.chart.repo}${w.chart.constraint ? ` ${w.chart.constraint}` : ""}`;
  }
  if (w.git) return w.git.repo;
  if (w.provider) return w.provider.name;
  return "unknown source";
}

function BeaconList({ ns }: { ns: string }) {
  const state = useApi(() => api.beacons(ns), [ns]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Beacons</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        What Hecate watches. A Beacon sees a new version and emits a Bundle.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(beacons) =>
            beacons.length === 0 ? (
              <p className="text-sm text-[var(--muted-foreground)]">
                No Beacons in <code>{ns}</code>. Nothing here emits Bundles, so no Gate has
                anything to admit.
              </p>
            ) : (
              <ul className="divide-y divide-[var(--border)] text-sm">
                {beacons.map((b: Beacon) => (
                  <li key={b.metadata.name} className="flex items-baseline gap-3 py-2.5">
                    <Link
                      href={{
                        pathname: "/beacons/",
                        query: { name: b.metadata.name, namespace: ns },
                      }}
                      className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                    >
                      {b.metadata.name}
                    </Link>
                    <span className="truncate text-[var(--muted-foreground)]">
                      {(b.spec.watch ?? []).map(describeWatch).join(", ") || "watches nothing"}
                    </span>
                    {/* A suspended Beacon looks identical to a quiet one until
                        you say so, and "nothing has shipped for a week" is
                        usually this. */}
                    {b.spec.suspend && (
                      <span className="ml-auto shrink-0 text-[var(--unknown)]">suspended</span>
                    )}
                    {!b.spec.suspend && b.status?.latestBundle && (
                      <span className="ml-auto shrink-0 text-[var(--muted-foreground)]">
                        {b.status.latestBundle}
                      </span>
                    )}
                  </li>
                ))}
              </ul>
            )
          }
        </Panel>
      </div>
    </div>
  );
}
