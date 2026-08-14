"use client";

import { useEffect, useState } from "react";
import { ArrowRight, CheckCircle2, Clock, ShieldCheck, XCircle } from "lucide-react";

import { api, AuditEntry } from "@/lib/api";
import { useQueryParam } from "@/lib/browser";

/**
 * The audit trail: what happened, to what, on whose say-so, and against which
 * evidence.
 *
 * Refusals are shown as prominently as crossings, and that is the whole point.
 * A page listing everything that shipped is a deployment log; what makes this
 * an audit trail is that it also holds what was stopped and by what — which is
 * usually the question being asked when someone opens it.
 */
export default function AuditPage() {
  const namespace = useQueryParam("namespace", "default");
  const [entries, setEntries] = useState<AuditEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api
      .audit(namespace)
      .then((e) => live && setEntries(e))
      .catch((e) => live && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      live = false;
    };
  }, [namespace]);

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-2xl font-semibold">Audit</h1>
        <p className="text-[var(--muted-foreground)]">
          Every crossing, refusal and approval in {namespace}, newest first.
        </p>
      </header>

      {error && (
        <p className="rounded-md border border-red-500/40 bg-red-500/10 p-3 text-sm">{error}</p>
      )}

      {entries === null && !error && <p className="text-[var(--muted-foreground)]">Loading…</p>}

      {entries?.length === 0 && (
        <p className="text-[var(--muted-foreground)]">
          Nothing has crossed a Gate in {namespace} yet.
        </p>
      )}

      <ol className="space-y-3">
        {entries?.map((e, i) => (
          <li
            key={`${e.passage ?? e.bundle}-${e.at}-${i}`}
            className="rounded-lg border border-[var(--border)] p-3"
          >
            <div className="flex flex-wrap items-center gap-2">
              <Marker kind={e.kind} />
              <span className="font-medium">{e.bundle ?? "—"}</span>
              <ArrowRight className="size-4 text-[var(--muted-foreground)]" aria-hidden />
              <span className="font-medium">{e.gate}</span>
              <span className="ml-auto text-sm text-[var(--muted-foreground)]">
                {new Date(e.at).toLocaleString()}
              </span>
            </div>

            <p className="pt-1 text-sm text-[var(--muted-foreground)]">
              {/* An automatic Gate has no actor, and saying so beats an empty
                  cell that reads as missing data. */}
              {e.actor ? `by ${e.actor}` : "automatically"}
              {e.verified === true && " · verified"}
              {e.verified === false && " · not verified"}
              {/* The digest, not the Bundle name, is what shipped. Truncated
                  because the first bytes identify it and the rest is noise. */}
              {e.digest && ` · ${e.digest.slice(0, 19)}`}
            </p>

            {e.detail && (
              <p className="pt-1 text-sm text-red-400 break-words">{e.detail}</p>
            )}

            {e.evidence && (e.evidence.trail || e.evidence.verdict) && (
              <p className="pt-1 text-sm">
                <span className="text-[var(--muted-foreground)]">evidence: </span>
                {e.evidence.verdict && <span>{e.evidence.verdict}</span>}
                {typeof e.evidence.risk === "number" && <span> · risk {e.evidence.risk}</span>}
                {e.evidence.url ? (
                  <a
                    className="pl-1 underline"
                    href={e.evidence.url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    trail
                  </a>
                ) : (
                  e.evidence.trail && (
                    <span className="pl-1 text-[var(--muted-foreground)]">{e.evidence.trail}</span>
                  )
                )}
                {e.evidence.blockers?.length ? (
                  <span className="text-red-400"> · {e.evidence.blockers.join(", ")}</span>
                ) : null}
              </p>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}

function Marker({ kind }: { kind: AuditEntry["kind"] }) {
  const map = {
    crossed: { Icon: CheckCircle2, tone: "text-green-500", label: "crossed" },
    refused: { Icon: XCircle, tone: "text-red-500", label: "refused" },
    running: { Icon: Clock, tone: "text-amber-500", label: "running" },
    approved: { Icon: ShieldCheck, tone: "text-blue-400", label: "approved" },
  } as const;
  const { Icon, tone, label } = map[kind];
  return (
    <span className={`flex items-center gap-1 text-sm ${tone}`}>
      <Icon className="size-4" aria-hidden />
      {label}
    </span>
  );
}
