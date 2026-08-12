"use client";

import { api, type Bundle } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";

export default function Bundles() {
  const ns = useNamespace();
  const state = useApi(() => api.bundles(ns), [ns]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Bundles</h1>
      <p className="mt-1 text-sm text-[var(--muted)]">
        An immutable, content-addressed set of artifact versions. The unit that moves.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(bundles) =>
            bundles.length === 0 ? (
              <p className="text-sm text-[var(--muted)]">
                No Bundles in <code>{ns}</code>.
              </p>
            ) : (
              <ul className="divide-y divide-[var(--line)] text-sm">
                {bundles.map((b: Bundle) => (
                  <li key={b.metadata.name} className="flex items-baseline gap-3 py-2.5">
                    <span className="font-medium">{b.spec.alias || b.metadata.name}</span>
                    <span className="text-[var(--muted)]">{b.spec.beacon}</span>
                    {b.status?.approvedFor?.length ? (
                      <span className="ml-auto text-[var(--muted)]">
                        approved for {b.status.approvedFor.join(", ")}
                      </span>
                    ) : null}
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
