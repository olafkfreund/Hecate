"use client";

import { useState } from "react";
import Link from "next/link";
import { Check, Loader2, ShieldCheck } from "lucide-react";
import { api, ApiError, Unauthenticated, type Explanation } from "@/lib/api";
import { Panel, useLiveAll } from "@/components/loader";
import { NamespaceGroups } from "@/components/groups";

interface Pending {
  // Which namespace's Gate is waiting. Two teams can each have a `production`
  // Gate, and approving is not a thing to do to the wrong one.
  namespace: string;
  gate: string;
  bundle: string;
}

/**
 * What is waiting on a human.
 *
 * The list comes from each Gate's own explanation, filtered on the stable
 * `AwaitingApproval` code rather than on the wording of the reason — the UI
 * does not re-decide who needs approving, because that rule lives in pkg/ops
 * and a second copy of it here would eventually disagree (D32).
 */
export default function Approvals() {
  const [approved, setApproved] = useState(0);

  const state = useLiveAll(async () => {
    const gates = await api.gates();
    // Sequential rather than Promise.all: a handful of Gates, and a burst of
    // parallel requests against an API that does a TokenReview and a
    // SubjectAccessReview per call is a poor trade for a few milliseconds.
    const pending: Pending[] = [];
    for (const g of gates) {
      const ns = g.metadata.namespace;
      const ex: Explanation = await api.explain(ns, g.metadata.name);
      for (const w of ex.waiting ?? []) {
        if (w.kind === "AwaitingApproval") {
          pending.push({ namespace: ns, gate: ex.gate, bundle: w.bundle });
        }
      }
    }
    return pending;
  }, [approved]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Approvals</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        Bundles that cannot cross until someone says so.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(pending) => (
              <NamespaceGroups
                items={pending}
                namespaceOf={(p: Pending) => p.namespace}
                empty={
                  <p className="text-sm text-[var(--muted-foreground)]">Nothing is waiting on you.</p>
                }
              >
                {(group) => (
                  <ul className="divide-y divide-[var(--border)]">
                    {group.map((p) => (
                      <Row
                        key={`${p.namespace}/${p.gate}/${p.bundle}`}
                        pending={p}
                        namespace={p.namespace}
                        onApproved={() => setApproved((n) => n + 1)}
                      />
                    ))}
                  </ul>
                )}
              </NamespaceGroups>
          )}
        </Panel>
      </div>
    </div>
  );
}

function Row({
  pending,
  namespace,
  onApproved,
}: {
  pending: Pending;
  namespace: string;
  onApproved: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  async function approve() {
    setBusy(true);
    setFailed(null);
    try {
      await api.approve(namespace, pending.bundle, pending.gate);
      onApproved();
    } catch (e) {
      if (e instanceof Unauthenticated) setFailed("Your session has expired. Sign in again.");
      else if (e instanceof ApiError && e.status === 403)
        // Not a fault. Approving is a separate permission from promoting, and
        // the whole point is that one person cannot hold both.
        setFailed(
          "You may not approve for this Gate. Approving is a separate permission " +
            "from promoting — that separation is the control, not a misconfiguration.",
        );
      else setFailed(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="py-3">
      <div className="flex items-center gap-3 text-sm">
        <Link
          href={{ pathname: "/bundles/", query: { name: pending.bundle, namespace } }}
          className="font-medium underline decoration-[var(--border)] underline-offset-4 hover:decoration-current"
        >
          {pending.bundle}
        </Link>
        <span className="text-[var(--muted-foreground)]">→ {pending.gate}</span>

        <button
          onClick={approve}
          disabled={busy}
          className="ml-auto flex items-center gap-1.5 rounded-md bg-[var(--primary)] px-3 py-1.5 text-sm font-medium text-[var(--primary-foreground)] disabled:opacity-50"
        >
          {busy ? (
            <Loader2 size={14} className="animate-spin" aria-hidden />
          ) : (
            <Check size={14} aria-hidden />
          )}
          Approve for {pending.gate}
        </button>
      </div>

      <p className="mt-1 flex items-center gap-1.5 text-xs text-[var(--muted-foreground)]">
        <ShieldCheck size={12} aria-hidden />
        Approving records you as the approver. It does not cross the Gate.
      </p>

      {failed && (
        <p role="alert" className="mt-2 text-sm text-[var(--destructive)]">
          {failed}
        </p>
      )}
    </li>
  );
}
