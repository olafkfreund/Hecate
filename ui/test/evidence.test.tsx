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

describe("the ITSM change on a crossing", () => {
  const withChange = (change: Evidence["change"]): Evidence => ({ ...held, change });

  it("names the change request, its approval and its state", async () => {
    vi.spyOn(api, "evidence").mockResolvedValue(
      withChange({
        number: "CHG0033184",
        state: "new",
        approval: "not requested",
        risk: "3",
        on_hold: false,
        short_description: "Rollback Oracle Version",
        found: true,
      }),
    );

    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);

    // The verdict says the crossing is held. This says which ticket has to move
    // for it not to be, which is the next action rather than the diagnosis.
    await waitFor(() => expect(screen.getByText("CHG0033184")).toBeDefined());
    expect(container.textContent).toContain("not requested");
    expect(container.textContent).toContain("Rollback Oracle Version");
  });

  it("says when an approved change is still on hold", async () => {
    vi.spyOn(api, "evidence").mockResolvedValue(
      withChange({
        number: "CHG1",
        state: "scheduled",
        approval: "approved",
        on_hold: true,
        found: true,
      }),
    );

    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);

    // Approved and not actionable is the one combination the approval word
    // alone describes wrongly.
    await waitFor(() => expect(screen.getByText("CHG1")).toBeDefined());
    expect(container.textContent).toContain("on hold");
  });

  it("distinguishes a missing change from an unapproved one", async () => {
    vi.spyOn(api, "evidence").mockResolvedValue(
      withChange({ number: "CHG9", state: "", approval: "", on_hold: false, found: false }),
    );

    render(<EvidencePanel namespace="uidemo" bundle="b1" />);

    // Different answers with different fixes, and only one of them is
    // somebody's to sign.
    await waitFor(() =>
      expect(screen.getByText(/does not exist in ServiceNow/)).toBeDefined(),
    );
  });

  it("shows nothing when no ITSM check was recorded", async () => {
    vi.spyOn(api, "evidence").mockResolvedValue(held);

    const { container } = render(<EvidencePanel namespace="uidemo" bundle="b1" />);

    await waitFor(() => expect(screen.getByText("hold")).toBeDefined());
    // The row itself, not merely its content: an empty change renders empty
    // spans and no text, so asserting on text alone passes while a blank row
    // sits on the page — which is what a mutation caught here.
    expect(screen.queryByLabelText("Change request")).toBeNull();
    expect(container.textContent).not.toContain("ServiceNow");
  });
});
