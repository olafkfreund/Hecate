"use client";

import { CircleAlert, CircleCheck, Eye, Package, PauseCircle, Timer } from "lucide-react";
import { api, type Beacon, type WatchSource } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { NamespaceGroups } from "@/components/groups";
import { Card, Grid, Meta, Note, Pill, ago } from "@/components/card";
import { useQueryParam } from "@/lib/browser";
import { BeaconDetail } from "./detail";

export default function Beacons() {
  const selected = useQueryParam("name", "");
  if (selected) return <BeaconDetail />;
  return <BeaconList />;
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

function BeaconList() {
  const state = useLiveAll(() => api.beacons(), []);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Beacons</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        What Hecate watches. A Beacon sees a new version and emits a Bundle.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(beacons) => (
            <NamespaceGroups
              items={beacons}
              namespaceOf={(b: Beacon) => b.metadata.namespace}
              empty={
                <p className="text-sm text-[var(--muted-foreground)]">
                  No Beacons anywhere you can read. Nothing emits Bundles, so no
                  Gate has anything to admit.
                </p>
              }
            >
              {(group) => (
                <Grid>
                  {group.map((b: Beacon) => (
                    <BeaconCard
                      key={`${b.metadata.namespace}/${b.metadata.name}`}
                      beacon={b}
                      namespace={b.metadata.namespace}
                    />
                  ))}
                </Grid>
              )}
            </NamespaceGroups>
          )}
        </Panel>
      </div>
    </div>
  );
}

function BeaconCard({ beacon, namespace }: { beacon: Beacon; namespace: string }) {
  const watch = beacon.spec.watch ?? [];
  // A Beacon reports its own trouble in conditions, and until now the list
  // showed none of it: a Beacon that cannot reach its registry looked exactly
  // like one whose registry has nothing new.
  const bad = (beacon.status?.conditions ?? []).find(
    (c) => c.type === "Ready" && c.status !== "True",
  );

  return (
    <Card
      href={{ pathname: "/beacons/", query: { name: beacon.metadata.name, namespace } }}
      title={beacon.metadata.name}
      edge={bad ? "border-l-[var(--destructive)]" : "border-l-[var(--healthy)]"}
      right={
        beacon.spec.suspend ? (
          <Pill icon={PauseCircle} tone="warn">
            suspended
          </Pill>
        ) : bad ? (
          <Pill icon={CircleAlert} tone="bad">
            not ready
          </Pill>
        ) : (
          <Pill icon={CircleCheck} tone="good">
            watching
          </Pill>
        )
      }
    >
      <ul className="mt-1 space-y-0.5 text-sm text-[var(--muted-foreground)]">
        {watch.length === 0 ? (
          <li>watches nothing — it will never emit a Bundle</li>
        ) : (
          watch.map((w, i) => (
            <li key={i} className="truncate" title={describeWatch(w)}>
              {describeWatch(w)}
            </li>
          ))
        )}
      </ul>

      <Meta>
        {beacon.spec.interval && <Pill icon={Timer}>every {beacon.spec.interval}</Pill>}
        {ago(beacon.status?.lastPolled) && (
          <Pill icon={Eye}>looked {ago(beacon.status?.lastPolled)}</Pill>
        )}
        {beacon.status?.latestBundle && (
          <Pill
            icon={Package}
            href={{
              pathname: "/bundles/",
              query: { name: beacon.status.latestBundle, namespace },
            }}
          >
            {beacon.status.latestBundle}
          </Pill>
        )}
      </Meta>

      {bad && <Note tone="bad">{bad.message ?? bad.reason ?? "not ready"}</Note>}
    </Card>
  );
}
