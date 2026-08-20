"use client";

import Link from "next/link";
import { api, type Passage } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { took } from "@/lib/timeline";
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

function PassageList({ ns }: { ns: string }) {
  const state = useApi(() => api.passages(ns), [ns]);

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
              <ul className="divide-y divide-[var(--border)] text-sm">
                {passages.map((p: Passage) => {
                  const duration = took(p.status?.startedAt, p.status?.finishedAt);
                  return (
                    <li key={p.metadata.name} className="py-2.5">
                      <div className="flex items-baseline gap-3">
                        <Link
                          href={{
                            pathname: "/passages/",
                            query: { name: p.metadata.name, namespace: ns },
                          }}
                          className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                        >
                          {p.spec.bundle}
                        </Link>
                        <span className="text-[var(--muted-foreground)]">→ {p.spec.gate}</span>
                        {duration && (
                          <span className="text-[var(--muted-foreground)]">{duration}</span>
                        )}
                        <span className="ml-auto">{p.status?.phase ?? "Pending"}</span>
                      </div>
                      {p.status?.message && (
                        <p className="mt-1 text-[var(--muted-foreground)]">{p.status.message}</p>
                      )}
                    </li>
                  );
                })}
              </ul>
            )
          }
        </Panel>
      </div>
    </div>
  );
}
