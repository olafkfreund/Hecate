"use client";

import { Boxes, CircleCheck, CircleX, Clock, ShieldCheck } from "lucide-react";
import { api, type Bundle } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { Card, Grid, Meta, Note, Pill, ago } from "@/components/card";
import { NamespaceGroups } from "@/components/groups";
import { useQueryParam } from "@/lib/browser";
import { BundleDetail } from "./detail";

export default function Bundles() {
  const selected = useQueryParam("name", "");
  // One route, two views. A static export cannot have /bundles/[name] without
  // knowing every Bundle at build time, and Bundle names are content
  // addresses — there is no knowing them.
  if (selected) return <BundleDetail />;
  return <BundleList />;
}

function BundleList() {
  const state = useLiveAll(() => api.bundles(), []);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Bundles</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        An immutable, content-addressed set of artifact versions. The unit that moves.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(bundles) => (
            <NamespaceGroups
              items={bundles}
              namespaceOf={(b: Bundle) => b.metadata.namespace}
              empty={
                <p className="text-sm text-[var(--muted-foreground)]">
                  No Bundles anywhere you can read. Either none exist yet, or your
                  Kubernetes user cannot see the namespaces holding them.
                </p>
              }
            >
              {(group) => (
                <Grid>
                  {group.map((b: Bundle) => (
                    <BundleCard
                      key={`${b.metadata.namespace}/${b.metadata.name}`}
                      bundle={b}
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

function BundleCard({ bundle, namespace }: { bundle: Bundle; namespace: string }) {
  const cleared = bundle.status?.cleared ?? [];
  const blocked = bundle.status?.blocked ?? [];
  const approved = bundle.status?.approvedFor ?? [];
  const artifacts = bundle.spec.artifacts ?? [];

  // How far it has actually got, which is the question every Bundle row is
  // opened to answer and the one thing the old row never said.
  const furthest = cleared[cleared.length - 1]?.gate;

  return (
    <Card
      href={{ pathname: "/bundles/", query: { name: bundle.metadata.name, namespace } }}
      title={bundle.spec.alias || bundle.metadata.name}
      subtitle={bundle.spec.beacon}
      edge={
        blocked.length > 0
          ? "border-l-[var(--destructive)]"
          : cleared.length > 0
            ? "border-l-[var(--healthy)]"
            : "border-l-[var(--border)]"
      }
      right={
        furthest ? (
          <Pill icon={CircleCheck} tone="good">
            in {furthest}
          </Pill>
        ) : (
          <Pill tone="quiet">not yet crossed</Pill>
        )
      }
    >
      <Meta>
        {artifacts.length > 0 && (
          <Pill icon={Boxes}>
            {artifacts.length} {artifacts.length === 1 ? "artifact" : "artifacts"}
          </Pill>
        )}
        {approved.length > 0 && (
          <Pill icon={ShieldCheck} tone="warn">
            approved for {approved.map((a) => a.gate).join(", ")}
          </Pill>
        )}
        {blocked.length > 0 && (
          <Pill icon={CircleX} tone="bad">
            blocked at {blocked[blocked.length - 1].gate}
          </Pill>
        )}
        {ago(bundle.metadata.creationTimestamp) && (
          <Pill icon={Clock}>emitted {ago(bundle.metadata.creationTimestamp)}</Pill>
        )}
      </Meta>

      {/* Why it stopped. A Bundle sitting still is the common case someone
          opens this page for, and the reason lived a click away. */}
      {blocked.length > 0 && blocked[blocked.length - 1].reason && (
        <Note tone="bad">{blocked[blocked.length - 1].reason}</Note>
      )}
    </Card>
  );
}
