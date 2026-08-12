"use client";

import { api, type Passage } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";

export default function Passages() {
  const ns = useNamespace();
  const state = useApi(() => api.passages(ns), [ns]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Passages</h1>
      <p className="mt-1 text-sm text-[var(--muted)]">
        One attempt to move one Bundle through one Gate.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(passages) =>
            passages.length === 0 ? (
              <p className="text-sm text-[var(--muted)]">
                No Passages in <code>{ns}</code>.
              </p>
            ) : (
              <ul className="divide-y divide-[var(--line)] text-sm">
                {passages.map((p: Passage) => (
                  <li key={p.metadata.name} className="py-2.5">
                    <div className="flex items-baseline gap-3">
                      <span className="font-medium">{p.spec.bundle}</span>
                      <span className="text-[var(--muted)]">→ {p.spec.gate}</span>
                      <span className="ml-auto">{p.status?.phase ?? "Pending"}</span>
                    </div>
                    {p.status?.message && (
                      <p className="mt-1 text-[var(--muted)]">{p.status.message}</p>
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
