"use client";

import { useState } from "react";
import { CircleSlash, PauseCircle, PlayCircle, RefreshCw, TriangleAlert } from "lucide-react";
import {
  api,
  ApiError,
  Unauthenticated,
  type FluxResource,
} from "@/lib/api";
import { Panel, useLiveApi } from "@/components/loader";
import { HealthDot } from "@/components/health";
import { Card, Meta, Note, Pill } from "@/components/card";

/**
 * What this Gate's crossings actually depend on, and the three things an
 * operator does to it when something is wrong.
 *
 * Scoped to what the Gate watches rather than to everything Flux owns in the
 * namespace: a panel listing every Kustomization would invite someone to
 * suspend one this Gate has nothing to do with, and the API refuses that
 * anyway.
 */
export function FluxPanel({ namespace, gate }: { namespace: string; gate: string }) {
  const [changed, setChanged] = useState(0);
  const state = useLiveApi(() => api.flux(namespace, gate), [namespace, gate, changed]);

  return (
    <Panel state={state}>
      {(resources: FluxResource[]) =>
        resources.length === 0 ? null : (
          <section>
            <h2 className="text-sm font-medium">Flux</h2>
            <p className="mt-0.5 text-sm text-[var(--muted-foreground)]">
              What this Gate watches, and what it takes to move it.
            </p>

            <SuspensionBanner resources={resources} />

            <div className="mt-3 flex flex-col gap-3">
              {resources.map((r) => (
                <FluxCard
                  key={`${r.kind}/${r.name}`}
                  resource={r}
                  namespace={namespace}
                  gate={gate}
                  onChanged={() => setChanged((n) => n + 1)}
                />
              ))}
            </div>
          </section>
        )
      }
    </Panel>
  );
}

/**
 * A standing banner for as long as anything is suspended.
 *
 * Not a badge on the card and not a toast when it happens. A suspension
 * outlives the session that made it: someone pauses production to debug, is
 * interrupted, and every crossing afterwards reports success while changing
 * nothing — because the resource that would have applied it is not being
 * reconciled. It is the only Flux state that fails silently, so it is the only
 * one that gets a banner.
 */
function SuspensionBanner({ resources }: { resources: FluxResource[] }) {
  const suspended = resources.filter((r) => r.suspended);
  if (suspended.length === 0) return null;

  return (
    <div
      role="status"
      className="mt-3 flex items-start gap-2 rounded-lg border border-[var(--unknown)] bg-[var(--secondary)] p-3 text-sm"
    >
      <TriangleAlert size={16} className="mt-0.5 shrink-0 text-[var(--unknown)]" aria-hidden />
      <p>
        <span className="font-medium">
          {suspended.length === 1
            ? `${suspended[0].kind} ${suspended[0].name} is suspended.`
            : `${suspended.length} resources are suspended.`}
        </span>{" "}
        <span className="text-[var(--muted-foreground)]">
          Flux is not reconciling {suspended.length === 1 ? "it" : "them"}, so a crossing will
          report success and change nothing. Suspension is not in git — it stays until someone
          resumes it here or with <code>flux resume</code>.
        </span>
      </p>
    </div>
  );
}

function FluxCard({
  resource,
  namespace,
  gate,
  onChanged,
}: {
  resource: FluxResource;
  namespace: string;
  gate: string;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState<"suspend" | "reconcile" | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  const [asked, setAsked] = useState<string | null>(null);

  async function run(what: "suspend" | "reconcile") {
    setBusy(what);
    setFailed(null);
    try {
      if (what === "suspend") {
        await api.suspendFlux(namespace, gate, resource.kind, resource.name, !resource.suspended);
      } else {
        const { requestedAt } = await api.reconcileFlux(
          namespace,
          gate,
          resource.kind,
          resource.name,
        );
        setAsked(requestedAt);
      }
      onChanged();
    } catch (e) {
      if (e instanceof Unauthenticated) setFailed("Your session has expired. Sign in again.");
      else if (e instanceof ApiError && e.status === 403)
        // Not a fault. Operating Flux is a separate permission from promoting,
        // and deliberately so — the button stays visible rather than vanishing,
        // because a control that disappears teaches nobody that the right
        // exists or who to ask for it.
        setFailed(
          "You may not operate Flux here. It is a separate permission from promoting — " +
            "suspending stops every future deploy of this resource, and git will not undo it. " +
            "The role is hecate-flux-operator.",
        );
      else setFailed(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  const handled = asked !== null && resource.lastHandled === asked;

  return (
    <Card
      title={`${resource.kind} ${resource.name}`}
      edge={resource.suspended ? "border-l-[var(--unknown)]" : undefined}
      right={
        resource.missing ? (
          <Pill icon={CircleSlash} tone="warn">
            not in the cluster
          </Pill>
        ) : (
          <HealthDot health={resource.health} />
        )
      }
    >
      <Meta>
        {resource.suspended && (
          <Pill icon={PauseCircle} tone="warn">
            suspended
          </Pill>
        )}
        {resource.revision && (
          <span className="truncate text-[var(--muted-foreground)]" title={resource.revision}>
            {resource.revision}
          </span>
        )}
      </Meta>

      {!resource.missing && (
        <div className="mt-3 flex flex-wrap gap-2">
          <button
            onClick={() => run("suspend")}
            disabled={busy !== null}
            className={`flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-sm font-medium disabled:opacity-50 ${
              resource.suspended
                ? "border-[var(--healthy)] text-[var(--healthy)]"
                : "border-[var(--unknown)] text-[var(--unknown)]"
            }`}
          >
            {resource.suspended ? (
              <PlayCircle size={14} aria-hidden />
            ) : (
              <PauseCircle size={14} aria-hidden />
            )}
            {busy === "suspend" ? "…" : resource.suspended ? "Resume" : "Suspend"}
          </button>

          <button
            onClick={() => run("reconcile")}
            disabled={busy !== null || resource.suspended}
            title={
              resource.suspended
                ? "Suspended resources ignore a reconcile request. Resume it first."
                : undefined
            }
            className="flex items-center gap-1.5 rounded-md border border-[var(--border)] px-3 py-1.5 text-sm font-medium disabled:opacity-50"
          >
            <RefreshCw size={14} aria-hidden />
            {busy === "reconcile" ? "…" : "Reconcile now"}
          </button>
        </div>
      )}

      {asked && !failed && (
        <p className="mt-2 text-xs text-[var(--muted-foreground)]">
          {handled ? "Reconciled." : "Asked. Flux acts on its next turn."}
        </p>
      )}

      {resource.detail && <Note tone={resource.suspended ? "warn" : "quiet"}>{resource.detail}</Note>}

      {failed && (
        <p role="alert" className="mt-2 text-sm text-[var(--destructive)]">
          {failed}
        </p>
      )}
    </Card>
  );
}
