"use client";

import { CircleCheck, CircleX, Clock, ListChecks, Loader2, OctagonX, Timer, User } from "lucide-react";
import { api, type Passage } from "@/lib/api";
import { Panel, useLiveApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { took } from "@/lib/timeline";
import { Card, Meta, Note, Pill, Rows, ago } from "@/components/card";
import { PassageDetail } from "./detail";

export default function Passages() {
  const ns = useNamespace();
  const selected = useQueryParam("name", "");
  // One route, two views — the same shape as Bundles, and for the same reason:
  // a static export cannot have /passages/[name] without knowing every Passage
  // name at build time, and they are generated per crossing.
  if (selected) return <PassageDetail />;
  return <PassageList ns={ns} />;
}

/** The colour and word for each Passage phase. */
const phases: Record<string, { tone: "good" | "bad" | "busy" | "warn" | "quiet"; Icon: typeof CircleCheck }> = {
  Succeeded: { tone: "good", Icon: CircleCheck },
  Failed: { tone: "bad", Icon: CircleX },
  Aborted: { tone: "warn", Icon: OctagonX },
  Running: { tone: "busy", Icon: Loader2 },
  Pending: { tone: "quiet", Icon: Clock },
};

function PassageList({ ns }: { ns: string }) {
  const state = useLiveApi(() => api.passages(ns), [ns]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Passages</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        One attempt to move one Bundle through one Gate.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(passages) =>
            passages.length === 0 ? (
              <p className="text-sm text-[var(--muted-foreground)]">
                No Passages in <code>{ns}</code>.
              </p>
            ) : (
              // One column, not a grid: a failure message is a paragraph, and
              // three of them side by side is three columns of small print.
              <Rows>
                {passages.map((p: Passage) => (
                  <PassageCard key={p.metadata.name} passage={p} namespace={ns} />
                ))}
              </Rows>
            )
          }
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
