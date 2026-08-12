"use client";

import Link from "next/link";
import { api, type Bundle } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { BundleDetail } from "./detail";

export default function Bundles() {
  const ns = useNamespace();
  const selected = useQueryParam("name", "");
  // One route, two views. A static export cannot have /bundles/[name] without
  // knowing every Bundle at build time, and Bundle names are content
  // addresses — there is no knowing them.
  if (selected) return <BundleDetail />;
  return <BundleList ns={ns} />;
}

function BundleList({ ns }: { ns: string }) {
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
                    <Link
                      href={{ pathname: "/bundles/", query: { name: b.metadata.name, namespace: ns } }}
                      className="font-medium underline decoration-[var(--line)] underline-offset-4 hover:decoration-current"
                    >
                      {b.spec.alias || b.metadata.name}
                    </Link>
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
