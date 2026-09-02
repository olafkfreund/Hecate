"use client";

import Link from "next/link";
import { CircleCheck, CircleX, Clock, ListChecks, Loader2, OctagonX, Plus, Timer, User } from "lucide-react";
import { api, type Passage } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { NamespaceGroups } from "@/components/groups";
import { useQueryParam } from "@/lib/browser";
import { took } from "@/lib/timeline";
import { Card, Meta, Note, Pill, Rows, ago } from "@/components/card";
import { PassageDetail } from "./detail";

export default function Passages() {
  const selected = useQueryParam("name", "");
  // One route, two views — the same shape as Bundles, and for the same reason:
  // a static export cannot have /passages/[name] without knowing every Passage
  // name at build time, and they are generated per crossing.
  if (selected) return <PassageDetail />;
  return <PassageList />;
}

/** The colour and word for each Passage phase. */
const phases: Record<string, { tone: "good" | "bad" | "busy" | "warn" | "quiet"; Icon: typeof CircleCheck }> = {
  Succeeded: { tone: "good", Icon: CircleCheck },
  Failed: { tone: "bad", Icon: CircleX },
  Aborted: { tone: "warn", Icon: OctagonX },
  Running: { tone: "busy", Icon: Loader2 },
  Pending: { tone: "quiet", Icon: Clock },
};

function PassageList() {
  const state = useLiveAll(() => api.passages(), []);

  return (
    <div>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Passages</h1>
          <p className="mt-1 text-sm text-[var(--muted-foreground)]">
            One attempt to move one Bundle through one Gate.
          </p>
        </div>
        <Link
          href="/passages/new/"
          className="flex items-center gap-1.5 rounded-md border border-[var(--border)] px-3 py-1.5 text-sm hover:bg-[var(--secondary)]"
        >
          <Plus size={15} aria-hidden />
          Author a Passage
        </Link>
      </div>

      <div className="mt-6">
        <Panel state={state}>
          {(passages) => (
            <NamespaceGroups
              items={passages}
              namespaceOf={(p: Passage) => p.metadata.namespace}
              empty={
                <p className="text-sm text-[var(--muted-foreground)]">
                  No Passages anywhere you can read. Nothing has attempted a
                  crossing yet.
                </p>
              }
            >
              {(group) => (
                // One column, not a grid: a failure message is a paragraph, and
                // three of them side by side is three columns of small print.
                <Rows>
                  {group.map((p: Passage) => (
                    <PassageCard
                      key={`${p.metadata.namespace}/${p.metadata.name}`}
                      passage={p}
                      namespace={p.metadata.namespace}
                    />
                  ))}
                </Rows>
              )}
            </NamespaceGroups>
          )}
        </Panel>
      </div>
    </div>
  );
}

function PassageCard({ passage, namespace }: { passage: Passage; namespace: string }) {
  const phase = passage.status?.phase ?? "Pending";
  const { tone, Icon } = phases[phase] ?? phases.Pending;
  const steps = passage.status?.steps ?? [];
  const done = steps.filter((s) => s.phase === "Succeeded").length;
  const duration = took(passage.status?.startedAt, passage.status?.finishedAt);
  const edges = {
    good: "border-l-[var(--healthy)]",
    bad: "border-l-[var(--destructive)]",
    busy: "border-l-[var(--progressing)]",
    warn: "border-l-[var(--unknown)]",
    quiet: "border-l-[var(--border)]",
  };

  return (
    <Card
      href={{ pathname: "/passages/", query: { name: passage.metadata.name, namespace } }}
      title={passage.spec.bundle}
      subtitle={`→ ${passage.spec.gate}`}
      edge={edges[tone]}
      right={
        <Pill icon={Icon} tone={tone}>
          {phase}
        </Pill>
      }
    >
      <Meta>
        {/* How far it got, not just whether it finished. A Passage that failed
            on step one and one that failed on step nine are different
            problems, and the list called both "Failed". */}
        {steps.length > 0 && (
          <Pill icon={ListChecks}>
            {done}/{steps.length} steps
          </Pill>
        )}
        {duration && <Pill icon={Timer}>{duration}</Pill>}
        {passage.spec.actor && <Pill icon={User}>{passage.spec.actor}</Pill>}
        {ago(passage.status?.startedAt) && <Pill icon={Clock}>started {ago(passage.status?.startedAt)}</Pill>}
      </Meta>

      {passage.status?.message && (
        <Note tone={phase === "Failed" ? "bad" : "quiet"}>{passage.status.message}</Note>
      )}
    </Card>
  );
}
