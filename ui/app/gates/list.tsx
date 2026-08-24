"use client";

import { CircleDot, Clock, PauseCircle, Zap } from "lucide-react";
import { api, type Gate } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { NamespaceGroups } from "@/components/groups";
import { HealthDot, healthEdge } from "@/components/health";
import { PipelineGraph } from "@/components/graph";
import { Card, Grid, Meta, Note, Pill, ago } from "@/components/card";

/** Every Gate in one namespace: the pipeline, then each Gate in detail. */
export function GateList() {
  const state = useLiveAll(() => api.gates(), []);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Gates</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        An environment, and the threshold a Bundle must cross to enter it.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(gates) => (
            <NamespaceGroups
              items={gates}
              namespaceOf={(g: Gate) => g.metadata.namespace}
              empty={
                <p className="text-sm text-[var(--muted-foreground)]">
                  No Gates anywhere you can read. Either none exist yet, or your
                  Kubernetes user cannot see the namespaces holding them — Hecate
                  does not decide this; the cluster does.
                </p>
              }
            >
              {(group, namespace) => (
                <div className="space-y-6">
                  {/* One pipeline per namespace, not one for the cluster: a
                      Gate's `after` names Gates beside it, so a single diagram
                      spanning namespaces would draw a flow that does not
                      exist. */}
                  <PipelineGraph gates={group} namespace={namespace} />
                  <Grid>
                    {group.map((g: Gate) => (
                      <GateCard
                        key={`${g.metadata.namespace}/${g.metadata.name}`}
                        gate={g}
                        namespace={g.metadata.namespace}
                      />
                    ))}
                  </Grid>
                </div>
              )}
            </NamespaceGroups>
          )}
        </Panel>
      </div>
    </div>
  );
}

function GateCard({ gate, namespace }: { gate: Gate; namespace: string }) {
  const health = gate.status?.health;
  const current = gate.status?.current;
  const eligible = gate.status?.eligible ?? [];
  const running = gate.status?.activePassage;

  // Where this Gate admits from, and whether a human has to say yes. It is the
  // Gate's whole purpose and was visible only as a label on an arrow.
  const from = (gate.spec.admits ?? []).flatMap((a) => a.after ?? []);
  const beacons = (gate.spec.admits ?? []).map((a) => a.from.beacon).filter(Boolean);
  const needsApproval = (gate.spec.admits ?? []).some((a) => a.requireApproval);

  return (
    <Card
      href={{ pathname: "/gates/", query: { name: gate.metadata.name, namespace } }}
      title={gate.metadata.name}
      edge={healthEdge(health?.status)}
      right={<HealthDot health={health?.status} />}
    >
      <p className="mt-1 truncate text-sm text-[var(--muted-foreground)]" title={current?.bundle}>
        {current?.bundle ?? "nothing yet"}
        {/* How long it has been there. A Gate holding the same Bundle for three
            weeks and one that took it a minute ago look identical otherwise,
            and the difference is most of what "is this current?" means. */}
        {current?.enteredAt && ago(current.enteredAt) && (
          <span className="text-[var(--muted-foreground)]"> · {ago(current.enteredAt)}</span>
        )}
      </p>

      <Meta>
        {gate.spec.suspend && (
          <Pill icon={PauseCircle} tone="warn">
            suspended
          </Pill>
        )}
        {running && (
          <Pill
            icon={CircleDot}
            tone="busy"
            href={{ pathname: "/passages/", query: { name: running, namespace } }}
          >
            crossing
          </Pill>
        )}
        {eligible.length > 0 && !running && (
          <Pill icon={Clock}>
            {eligible.length} waiting
          </Pill>
        )}
        {/* Automatic is the surprising state, not the default one: it means
            this Gate crosses without anyone pressing anything. */}
        {gate.spec.auto && <Pill icon={Zap}>automatic</Pill>}
        {needsApproval && <Pill tone="warn">needs approval</Pill>}
      </Meta>

      {(beacons.length > 0 || from.length > 0) && (
        <p className="mt-2 text-xs text-[var(--muted-foreground)]">
          admits {beacons.join(", ") || "anything"}
          {from.length > 0 && ` after ${from.join(", ")}`}
        </p>
      )}

      {/* The reason, not just the colour. A card showing a red edge and nothing
          else sends every reader to the same second page to find out why. */}
      {health?.issues?.length ? <Note tone="bad">{health.issues.join(" · ")}</Note> : null}

      {eligible.length > 0 && (
        <Note>
          waiting: {eligible.slice(0, 3).join(", ")}
          {eligible.length > 3 && ` and ${eligible.length - 3} more`}
        </Note>
      )}
    </Card>
  );
}
