"use client";

import { useState } from "react";
import Link from "next/link";
import { ArrowLeft, CircleCheck, CircleDashed, CircleX, Loader2, OctagonX } from "lucide-react";
import { api, ApiError, Unauthenticated, type Passage, type StepStatus } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { useQueryParam } from "@/lib/browser";
import { took } from "@/lib/timeline";

/**
 * One Passage: what it is doing, step by step, and how to stop it.
 *
 * Reached by query parameter for the same reason Bundles are — a static export
 * has to know every route at build time, and Passage names are generated.
 */
export function PassageDetail() {
  const ns = useNamespace();
  const name = useQueryParam("name", "");
  const [aborted, setAborted] = useState(0);
  const state = useApi(() => api.passage(ns, name), [ns, name, aborted]);

  return (
    <div>
      <Link
        href="/passages/"
        className="mb-4 inline-flex items-center gap-1.5 text-sm text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
      >
        <ArrowLeft size={14} aria-hidden />
        Passages
      </Link>

      <Panel state={state}>
        {(p: Passage) => (
          <div className="space-y-8">
            <header>
              <div className="flex items-baseline gap-3">
                <h1 className="text-xl font-semibold tracking-tight">{p.metadata.name}</h1>
                <span className="text-sm text-[var(--muted-foreground)]">
                  {p.status?.phase ?? "Pending"}
                </span>
              </div>
              <p className="mt-1 text-sm text-[var(--muted-foreground)]">
                <Link
                  href={{ pathname: "/bundles/", query: { name: p.spec.bundle, namespace: ns } }}
                  className="underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                >
                  {p.spec.bundle}
                </Link>
                {" → "}
                <Link
                  href={{ pathname: "/gates/", query: { name: p.spec.gate, namespace: ns } }}
                  className="underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
                >
                  {p.spec.gate}
                </Link>
                {/* "automatically" rather than an empty space: a Gate that
                    crosses on its own has no actor, and that is an answer. */}
                <span> · {p.spec.actor ? `by ${p.spec.actor}` : "automatically"}</span>
                {took(p.status?.startedAt, p.status?.finishedAt) && (
                  <span> · took {took(p.status?.startedAt, p.status?.finishedAt)}</span>
                )}
              </p>
              {p.status?.message && <p className="mt-2 text-sm">{p.status.message}</p>}
            </header>

            <Steps steps={p.status?.steps ?? []} />

            <Abort
              namespace={ns}
              passage={p}
              onAborted={() => setAborted((n) => n + 1)}
            />
          </div>
        )}
      </Panel>
    </div>
  );
}

/**
 * One mark per StepPhase the CRD defines. Pending and Skipped are deliberately
 * absent: both are "nothing happened here", and the dashed fallback says that
 * without inventing a distinction the phases do not carry.
 */
const marks: Record<string, { Icon: typeof CircleCheck; tone: string }> = {
  Succeeded: { Icon: CircleCheck, tone: "text-[var(--healthy)]" },
  Failed: { Icon: CircleX, tone: "text-[var(--destructive)]" },
  Aborted: { Icon: OctagonX, tone: "text-[var(--unknown)]" },
  Running: { Icon: Loader2, tone: "text-[var(--progressing)]" },
};

function Steps({ steps }: { steps: StepStatus[] }) {
  if (steps.length === 0) {
    return (
      <section>
        <h2 className="text-sm font-medium">Steps</h2>
        <p className="mt-2 text-sm text-[var(--muted-foreground)]">
          No step has started yet.
        </p>
      </section>
    );
  }

  return (
    <section>
      <h2 className="text-sm font-medium">Steps</h2>
      <ol className="mt-3 space-y-3">
        {steps.map((s, i) => {
          const { Icon, tone } = marks[s.phase] ?? {
            Icon: CircleDashed,
            tone: "text-[var(--muted-foreground)]",
          };
          const duration = took(s.startedAt, s.finishedAt);
          return (
            <li key={`${s.uses}-${i}`} className="flex gap-3 text-sm">
              <Icon
                size={16}
                className={`mt-0.5 shrink-0 ${tone} ${s.phase === "Running" ? "animate-spin" : ""}`}
                aria-hidden
              />
              <div className="min-w-0">
                <p>
                  <span className="font-medium">{s.as || s.uses}</span>
                  {s.as && (
                    <span className="ml-2 font-mono text-xs text-[var(--muted-foreground)]">
                      {s.uses}
                    </span>
                  )}
                  <span className="ml-2 text-[var(--muted-foreground)]">{s.phase}</span>
                  {duration && (
                    <span className="ml-2 text-[var(--muted-foreground)]">{duration}</span>
                  )}
                  {/* Attempts are shown only when there was more than one. A
                      step that waits on an external system is invoked
                      repeatedly by design, and "1 attempt" on every other step
                      would bury the one that retried thirty times. */}
                  {typeof s.attempts === "number" && s.attempts > 1 && (
                    <span className="ml-2 text-[var(--muted-foreground)]">
                      {s.attempts} attempts
                    </span>
                  )}
                </p>
                {s.reason && (
                  <p className="mt-0.5 font-mono text-xs text-[var(--muted-foreground)]">
                    {s.reason}
                  </p>
                )}
                {s.message && (
                  <p className="mt-0.5 break-words text-[var(--muted-foreground)]">{s.message}</p>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

/** Terminal phases: a Passage in one of these has nothing left to stop. */
const settled = new Set(["Succeeded", "Failed", "Aborted"]);

function Abort({
  namespace,
  passage,
  onAborted,
}: {
  namespace: string;
  passage: Passage;
  onAborted: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  const phase = passage.status?.phase ?? "Pending";
  if (settled.has(phase)) return null;

  // Asked for already. The controller stops at the next step boundary rather
  // than mid-step, so there is a window where the Passage is still Running and
  // pressing again would do nothing — saying so beats a button that no-ops.
  if (passage.spec.abort) {
    return (
      <section>
        <h2 className="text-sm font-medium">Abort</h2>
        <p className="mt-2 text-sm text-[var(--muted-foreground)]">
          An abort has been requested. The Passage stops after the step that is running now.
        </p>
      </section>
    );
  }

  async function abort() {
    setBusy(true);
    setFailed(null);
    try {
      await api.abort(namespace, passage.metadata.name);
      onAborted();
    } catch (e) {
      if (e instanceof Unauthenticated) setFailed("Your session has expired. Sign in again.");
      else if (e instanceof ApiError && e.status === 403)
        setFailed(
          "You may not abort this Passage. Aborting is a separate permission from " +
            "promoting — starting a crossing does not imply being allowed to stop one part-way.",
        );
      else setFailed(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2 className="text-sm font-medium">Abort</h2>
      <p className="mt-2 text-sm text-[var(--muted-foreground)]">
        Stops the Passage after the current step. Steps that already ran are not undone — what
        they changed stays changed.
      </p>
      <button
        onClick={abort}
        disabled={busy}
        className="mt-3 flex items-center gap-1.5 rounded-md border border-[var(--destructive)] px-3 py-1.5 text-sm font-medium text-[var(--destructive)] disabled:opacity-50"
      >
        {busy ? (
          <Loader2 size={14} className="animate-spin" aria-hidden />
        ) : (
          <OctagonX size={14} aria-hidden />
        )}
        Abort this Passage
      </button>
      {failed && (
        <p role="alert" className="mt-3 text-sm text-[var(--destructive)]">
          {failed}
        </p>
      )}
    </section>
  );
}
