"use client";

import Link from "next/link";
import { api, type Gate } from "@/lib/api";
import { Panel, useApi, useNamespace } from "@/components/loader";
import { HealthDot } from "@/components/health";

export default function Gates() {
  const ns = useNamespace();
  const state = useApi(() => api.gates(ns), [ns]);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Gates</h1>
      <p className="mt-1 text-sm text-[var(--muted)]">
        An environment, and the threshold a Bundle must cross to enter it.
      </p>

      <div className="mt-6">
        <Panel state={state}>
          {(gates) =>
            gates.length === 0 ? (
              <p className="text-sm text-[var(--muted)]">
                No Gates in <code>{ns}</code>.
              </p>
            ) : (
              <table className="w-full text-left text-sm">
                <thead className="text-[var(--muted)]">
                  <tr className="border-b border-[var(--line)]">
                    <th className="py-2 font-medium">Gate</th>
                    <th className="py-2 font-medium">Current</th>
                    <th className="py-2 font-medium">Health</th>
                    <th className="py-2 font-medium">Eligible</th>
                  </tr>
                </thead>
                <tbody>
                  {gates.map((g: Gate) => (
                    <tr key={g.metadata.name} className="border-b border-[var(--line)]">
                      <td className="py-2.5 font-medium">
                        <Link
                          href={{ pathname: "/gates/", query: { name: g.metadata.name, namespace: ns } }}
                          className="underline decoration-[var(--line)] underline-offset-4 hover:decoration-current"
                        >
                          {g.metadata.name}
                        </Link>
                      </td>
                      <td className="py-2.5 text-[var(--muted)]">
                        {g.status?.current?.bundle ?? "—"}
                      </td>
                      <td className="py-2.5">
                        <HealthDot health={g.status?.health?.status} />
                      </td>
                      <td className="py-2.5 text-[var(--muted)]">
                        {g.status?.eligible?.length
                          ? g.status.eligible.join(", ")
                          : "nothing waiting"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          }
        </Panel>
      </div>
    </div>
  );
}
