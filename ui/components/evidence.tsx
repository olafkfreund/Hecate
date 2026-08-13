"use client";

import { ShieldCheck, ShieldAlert, ShieldQuestion } from "lucide-react";
import { api, type Control, type Evidence } from "@/lib/api";
import { useApi } from "@/components/loader";

/**
 * EvidencePanel is what an auditor would be shown: why this artifact was
 * allowed into production, and who allowed it.
 *
 * Loaded separately from the Bundle rather than folded into it, because this
 * one calls out to Fides — a Fides that is slow or down must not stop the page
 * rendering the Bundle. The panel says so itself instead.
 */
export function EvidencePanel({ namespace, bundle }: { namespace: string; bundle: string }) {
  const state = useApi(() => api.evidence(namespace, bundle), [namespace, bundle]);

  // Deliberately not the shared <Panel>: a failure here is one section being
  // unavailable, not the page failing, and Panel renders a whole-page error.
  if (state.loading) return null;
  if (state.error) {
    return (
      <Section>
        <Line icon="unknown">
          Fides did not answer, so there is no evidence to show — this is not the
          same as there being none.{" "}
          {state.error instanceof Error ? state.error.message : ""}
        </Line>
      </Section>
    );
  }

  const ev = state.data as Evidence;
  if (ev.unavailable) {
    return (
      <Section>
        <Line icon="unknown">{ev.unavailable}</Line>
      </Section>
    );
  }

  const v = ev.verdict;
  if (!v) return null;
  const held = v.recommendation !== "approve";

  return (
    <Section>
      <Line icon={held ? "bad" : "good"}>
        <span className="font-medium">{v.recommendation}</span>
        <span className="text-[var(--muted-foreground)]">
          {" "}
          — risk {v.risk_score}/100 ({v.risk_level})
        </span>
      </Line>
      {v.summary && <p className="mt-1 text-sm text-[var(--muted-foreground)]">{v.summary}</p>}

      <dl className="mt-4 grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-4">
        <Stat label="Controls passed" value={v.passed?.length ?? 0} />
        <Stat label="Failed" value={v.failed?.length ?? 0} />
        <Stat label="Missing evidence" value={v.missing_evidence?.length ?? 0} />
        <Stat label="Waived" value={v.waived?.length ?? 0} />
        <Stat label="Attestations" value={v.attestations.total} />
        <Stat label="Non-compliant" value={v.attestations.non_compliant} />
        <Stat label="Human approvers" value={v.approvals.human_approvers} />
        <Stat label="Four eyes" value={v.approvals.four_eyes ? "yes" : "no"} />
      </dl>

      <Controls title="Failed controls" controls={v.failed} />
      <Controls title="Missing evidence" controls={v.missing_evidence} />
      {/* Listed separately from passes: a waiver is a governed exception, and
          an auditor's first question about a green gate is what was waived. */}
      <Controls title="Waived" controls={v.waived} />

      {/* Segregation of duties is the "who allowed it" half of the question,
          and its violations name people rather than controls. */}
      {v.segregation_of_duties && (
        <div className="mt-4 text-sm">
          <p className="font-medium">Segregation of duties</p>
          <p className="mt-1 text-[var(--muted-foreground)]">
            committed by {v.segregation_of_duties.committer || "unknown"}
            {", approved by "}
            {v.segregation_of_duties.approvers?.join(", ") || "nobody"}
            {", deployed by "}
            {v.segregation_of_duties.deployers?.join(", ") || "nobody"}
          </p>
          {v.segregation_of_duties.violations?.map((x) => (
            <p key={x} className="mt-1 text-[var(--muted-foreground)]">
              → {x}
            </p>
          ))}
        </div>
      )}

      {ev.trail && (
        <p className="mt-4 font-mono text-xs text-[var(--muted-foreground)]">
          Fides trail {ev.trail}
          {ev.gate ? ` · via Gate ${ev.gate}` : ""}
        </p>
      )}
    </Section>
  );
}

function Section({ children }: { children: React.ReactNode }) {
  return (
    <section>
      <h2 className="text-sm font-medium">Evidence</h2>
      <div className="mt-2">{children}</div>
    </section>
  );
}

function Line({ icon, children }: { icon: "good" | "bad" | "unknown"; children: React.ReactNode }) {
  const Icon = icon === "good" ? ShieldCheck : icon === "bad" ? ShieldAlert : ShieldQuestion;
  const colour = icon === "good" ? "--healthy" : icon === "bad" ? "--degraded" : "--unknown";
  return (
    <p className="flex gap-2 text-sm">
      <Icon size={16} className={`mt-0.5 shrink-0 text-[var(${colour})]`} aria-hidden />
      <span>{children}</span>
    </p>
  );
}

function Stat({ label, value }: { label: string; value: number | string }) {
  return (
    <div>
      <dt className="text-xs text-[var(--muted-foreground)]">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  );
}

function Controls({ title, controls }: { title: string; controls?: Control[] }) {
  if (!controls?.length) return null;
  return (
    <div className="mt-4 text-sm">
      <p className="font-medium">{title}</p>
      <ul className="mt-1 space-y-1">
        {controls.map((c) => (
          <li key={c.control} className="text-[var(--muted-foreground)]">
            <span className="font-mono text-xs">{c.control}</span> {c.name}
            {/* The reasons are the actionable half: a control key is a thing to
                look up, a reason is a thing to fix. */}
            {(c.reasons ?? c.waived_reasons)?.length
              ? ` — ${(c.reasons ?? c.waived_reasons)!.join(", ")}`
              : ""}
            {c.approved_by ? ` (waived by ${c.approved_by})` : ""}
          </li>
        ))}
      </ul>
    </div>
  );
}
