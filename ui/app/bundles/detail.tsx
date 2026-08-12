"use client";

import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { api, type Bundle } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { Timeline } from "@/components/timeline";
import { describeArtifact } from "@/lib/timeline";

/** One Bundle: what it is, and how it got where it is. */
export function BundleDetail() {
  const ns = useNamespace();
  const name = useQueryParam("name", "");
  const state = useApi(() => api.bundle(ns, name), [ns, name]);

  return (
    <div>
      <Link
        href="/bundles/"
        className="mb-4 inline-flex items-center gap-1.5 text-sm text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
      >
        <ArrowLeft size={14} aria-hidden />
        Bundles
      </Link>

      <Panel state={state}>
        {(b: Bundle) => (
          <div className="space-y-8">
            <header>
              <h1 className="text-xl font-semibold tracking-tight">
                {b.spec.alias || b.metadata.name}
              </h1>
              <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                {b.spec.alias ? `${b.metadata.name} · ` : ""}
                from {b.spec.beacon ?? "an unknown Beacon"}
              </p>
              {b.status?.approvedFor?.length ? (
                <p className="mt-2 text-sm">
                  Approved for{" "}
                  <span className="font-medium">{b.status.approvedFor.join(", ")}</span>
                </p>
              ) : null}
            </header>

            <section>
              <h2 className="text-sm font-medium">What is in it</h2>
              <ul className="mt-2 space-y-1.5 text-sm">
                {(b.spec.artifacts ?? []).map((a, i) => {
                  const { what, detail } = describeArtifact(a);
                  return (
                    <li key={i}>
                      <span className="font-medium">{what}</span>
                      {/* The digest is what was deployed; the tag is only what
                          it was called at the time. */}
                      {detail && (
                        <span className="ml-2 break-all font-mono text-xs text-[var(--muted-foreground)]">
                          {detail}
                        </span>
                      )}
                    </li>
                  );
                })}
                {(b.spec.artifacts ?? []).length === 0 && (
                  <li className="text-[var(--muted-foreground)]">No artifacts recorded.</li>
                )}
              </ul>
            </section>

            <section>
              <h2 className="text-sm font-medium">How it got here</h2>
              <div className="mt-3">
                <Timeline bundle={b} />
              </div>
            </section>
          </div>
        )}
      </Panel>
    </div>
  );
}
