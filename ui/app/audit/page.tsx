"use client";

import { ArrowRight, CheckCircle2, Clock, ShieldCheck, XCircle } from "lucide-react";

import { api, AuditEntry } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { NamespaceTag } from "@/components/groups";

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
  // Panel and useApi rather than this page's own loading state, which is what
  // it had: the sign-in prompt, the forbidden explanation and the error box all
  // live in Panel, so a page loading by hand shows raw error text to someone
  // whose session merely expired — a dead end on the one page an auditor is
  // most likely to arrive at cold.
  const state = useLiveAll(() => api.audit(), []);

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-2xl font-semibold">Audit</h1>
        <p className="text-[var(--muted-foreground)]">
          Every crossing, refusal and approval you can see, newest first.
        </p>
      </header>

      <Panel state={state}>
        {(entries: AuditEntry[]) =>
          entries.length === 0 ? (
            <p className="text-[var(--muted-foreground)]">
              Nothing has crossed a Gate anywhere you can read yet.
            </p>
          ) : (
            <ol className="space-y-3">
              {entries.map((e, i) => (
                <li
                  key={`${e.namespace}/${e.passage ?? e.bundle}-${e.at}-${i}`}
                  className="rounded-lg border border-[var(--border)] p-3"
                >
                  {/* Tagged rather than grouped, unlike every other list: this
                      page is ordered by time, and time is the question it
                      answers. Grouping by namespace would sort the answer into
                      piles that each have to be read separately. */}
                  <div className="flex flex-wrap items-center gap-2">
                    <Marker kind={e.kind} />
                    <NamespaceTag namespace={e.namespace} />
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
                    <p className="pt-1 break-words text-sm text-[var(--destructive)]">{e.detail}</p>
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
                          <span className="pl-1 text-[var(--muted-foreground)]">
                            {e.evidence.trail}
                          </span>
                        )
                      )}
                      {e.evidence.blockers?.length ? (
                        <span className="text-[var(--destructive)]">
                          {" "}
                          · {e.evidence.blockers.join(", ")}
                        </span>
                      ) : null}
                    </p>
                  )}
                </li>
              ))}
            </ol>
          )
        }
      </Panel>
    </div>
  );
}

function Marker({ kind }: { kind: AuditEntry["kind"] }) {
  const map = {
    // The theme's tokens, not raw Tailwind. globals.css copies Fides' palette
    // whole so the two products look like one platform, and a literal green
    // here is a colour that answers to neither theme nor product.
    crossed: { Icon: CheckCircle2, tone: "text-[var(--healthy)]", label: "crossed" },
    refused: { Icon: XCircle, tone: "text-[var(--destructive)]", label: "refused" },
    running: { Icon: Clock, tone: "text-[var(--progressing)]", label: "running" },
    approved: { Icon: ShieldCheck, tone: "text-[var(--unknown)]", label: "approved" },
  } as const;
  const { Icon, tone, label } = map[kind];
  return (
    <span className={`flex items-center gap-1 text-sm ${tone}`}>
      <Icon className="size-4" aria-hidden />
      {label}
    </span>
  );
}
