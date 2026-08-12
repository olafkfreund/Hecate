/**
 * Captured from a real hecate-api, not invented.
 *
 * The types in lib/api.ts are hand-written against the Go structs and have been
 * wrong before — `Explanation` originally had `reason` and `remedy` where the
 * server sends `kind` and `fix`, which compiled perfectly and would have
 * rendered blanks. Fixtures copied from actual responses are the only thing
 * that catches that; ones written from the same memory as the types cannot.
 */
import type { Bundle, Explanation, Gate, Passage } from "@/lib/api";

export const gates: Gate[] = [
  {
    metadata: { name: "production", namespace: "uidemo" },
    spec: {
      admits: [{ from: { beacon: "podinfo" }, after: ["staging"], requireApproval: true }],
    },
    status: { health: { status: "NotApplicable" } },
  },
  {
    metadata: { name: "staging", namespace: "uidemo" },
    spec: { admits: [{ from: { beacon: "podinfo" } }] },
    status: { health: { status: "NotApplicable" }, eligible: ["podinfo-6b2"] },
  },
];

export const bundles: Bundle[] = [
  {
    metadata: { name: "podinfo-6b2", namespace: "uidemo" },
    spec: { beacon: "podinfo", alias: "wandering-owl" },
    status: {},
  },
];

/** A Bundle with a history: emitted, cleared one Gate, refused at another. */
export const travelled: Bundle = {
  metadata: {
    name: "podinfo-6b2",
    namespace: "uidemo",
    creationTimestamp: "2026-08-10T09:00:00Z",
  },
  spec: {
    beacon: "podinfo",
    alias: "wandering-owl",
    artifacts: [
      {
        image: {
          repo: "ghcr.io/stefanprodan/podinfo",
          tag: "6.14.1",
          digest: "sha256:4a6f31e7c48b0fb7f3848479c9278284362ca590ee8ee06a377971f2af22464b",
        },
      },
    ],
  },
  status: {
    approvedFor: ["production"],
    // Deliberately out of order relative to `blocked`, so the timeline has to
    // sort rather than concatenate.
    cleared: [
      { gate: "staging", at: "2026-08-10T09:05:00Z", actor: "controller", passage: "staging-vck6g" },
    ],
    blocked: [
      {
        gate: "production",
        at: "2026-08-10T09:02:00Z",
        actor: "olaf@hecate.test",
        reason: "has not cleared staging",
      },
    ],
  },
};

export const passages: Passage[] = [
  {
    metadata: { name: "staging-vck6g", namespace: "uidemo" },
    spec: { gate: "staging", bundle: "podinfo-6b2", actor: "olaf@hecate.test" },
    status: { phase: "Succeeded", steps: [{ uses: "http", phase: "Succeeded" }] },
  },
];

export const explanation: Explanation = {
  gate: "staging",
  namespace: "uidemo",
  state: "Ready",
  summary: "1 Bundle ready to cross; this Gate does not cross automatically",
  blockers: [
    {
      kind: "AwaitingRequest",
      detail: "spec.auto is false, so a crossing must be requested",
      fix: "hecate promote staging --bundle podinfo-6b2",
    },
  ],
  eligible: ["podinfo-6b2"],
  health: "NotApplicable",
};
