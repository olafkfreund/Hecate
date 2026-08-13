import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { EvidencePanel } from "@/components/evidence";
import { api, ApiError, type Evidence } from "@/lib/api";

/**
 * The verdict body is evidance-vault's own change-gate output, key for key.
 * Written from Hecate's TypeScript types it would agree with them by
 * construction and prove nothing.
 */
const held: Evidence = {
  bundle: "b1",
  namespace: "uidemo",
  digest: "sha256:abc",
  trail: "91b2ffff-0000-4000-8000-00000000abcd",
  gate: "production",
  verdict: {
    recommendation: "hold",
    approved: false,
    risk_score: 45,
    risk_level: "medium",
    passed: ["CC8.1"],
    failed: [{ control: "CC7.2", name: "Vulnerability scanning", reasons: ["failed vuln-scan"] }],
    missing_evidence: [],
    waived: [
      { control: "CC9.9", name: "Pen test", waived_reasons: ["missing pentest"], approved_by: "ciso@acme.example" },
    ],
    attestations: { total: 4, non_compliant: 1 },
    approvals: { count: 1, human_approvers: 1, four_eyes: false, approvers: ["olaf@acme.example"] },
    segregation_of_duties: {
      committer: "olaf@acme.example",
      approvers: ["olaf@acme.example"],
      deployers: [],
      compliant: false,
      violations: ["committer and approver are the same person"],
    },
    summary: "Controls failed.",
  },
};

describe("the evidence panel", () => {
  // Braces matter: an arrow that returns the spy hands vitest a teardown
  // function, which it then calls — invoking the real api.evidence() with no
  // arguments after the mock has been restored. setup.ts already restores.
  beforeEach(() => {
    vi.spyOn(api, "evidence").mockResolvedValue(held);
  });

  it("answers why it was allowed and who allowed it", async () => {
    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);
    await waitFor(() => expect(screen.getByText("Evidence")).toBeDefined());

    expect(container.textContent).toContain("hold");
    expect(container.textContent).toContain("risk 45/100");
    // A control key is a thing to look up; a reason is a thing to fix.
    expect(container.textContent).toContain("failed vuln-scan");
    // The "who" half. One person doing both is the finding, not a gap.
    expect(container.textContent).toContain("olaf@acme.example");
    expect(container.textContent).toContain("committer and approver are the same person");
    // A waiver is a governed exception, not a pass — an auditor asks about it
    // first, so it must not be silently folded into the green count.
    expect(container.textContent).toContain("waived by ciso@acme.example");
    expect(container.textContent).toContain("91b2ffff-0000-4000-8000-00000000abcd");
  });

  it("says why there is no evidence rather than rendering empty", async () => {
    vi.spyOn(api, "evidence").mockResolvedValue({
      bundle: "b1",
      namespace: "uidemo",
      unavailable: "no Gate in this namespace records evidence in Fides",
    });

    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);
    // Nothing to show and a clean bill of health are opposite answers.
    await waitFor(() =>
      expect(container.textContent).toContain("no Gate in this namespace records evidence"),
    );
  });

  it("does not pass a Fides outage off as an absence of evidence", async () => {
    vi.spyOn(api, "evidence").mockRejectedValue(new ApiError(502, "bad gateway"));

    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);
    await waitFor(() => expect(container.textContent).toContain("Fides did not answer"));
    expect(container.textContent).toContain("not the same as there being none");
  });
});
