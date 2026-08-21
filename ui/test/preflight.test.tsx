import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Gates from "@/app/gates/page";
import { api, type Explanation, type Preflight } from "@/lib/api";

const blocked: Explanation = {
  gate: "production",
  namespace: "demo",
  state: "Open",
  summary: "1 Bundle may cross",
  eligible: ["podtato-head-fa9"],
};

function atGate(checks: Preflight[]) {
  window.history.replaceState({}, "", "/gates/?name=production&namespace=demo");
  vi.spyOn(api, "explain").mockResolvedValue(blocked);
  vi.spyOn(api, "flux").mockResolvedValue([]);
  vi.spyOn(api, "preflight").mockResolvedValue(checks);
}

describe("the evidence pre-flight", () => {
  it("says a Bundle would be refused, before anyone presses Cross", async () => {
    atGate([
      {
        bundle: "podtato-head-fa9",
        compliant: false,
        missing: ["servicenow-change"],
        policies: ["change-control"],
      },
    ]);

    render(<Gates />);

    // The alternative is what the demo currently does: press Cross, create a
    // Passage, watch it fail on the evidence gate, and leave a failed crossing
    // in the record for something nobody could have known.
    await waitFor(() => expect(screen.getByText("would be refused")).toBeDefined());
  });

  it("names what is missing, since a refusal nobody can act on is just a status", async () => {
    atGate([
      {
        bundle: "podtato-head-fa9",
        compliant: false,
        missing: ["sbom", "servicenow-change"],
        policies: ["sox"],
      },
    ]);

    const { container } = render(<Gates />);

    await waitFor(() => expect(container.textContent).toContain("needs sbom, servicenow-change"));
    expect(container.textContent).toContain("for sox");
  });

  it("still offers the button, because Hecate does not decide this", async () => {
    atGate([{ bundle: "podtato-head-fa9", compliant: false, missing: ["sbom"] }]);

    render(<Gates />);

    await waitFor(() => expect(screen.getByText("would be refused")).toBeDefined());
    // The evidence gate decides, at crossing time. A UI that refused on its own
    // would be a second copy of a rule that lives in one place — and would be
    // wrong the moment the missing attestation arrives.
    const button = screen.getByText(/Cross production/).closest("button") as HTMLButtonElement;
    expect(button.disabled).toBe(false);
  });

  it("says ready when the evidence is there", async () => {
    atGate([{ bundle: "podtato-head-fa9", compliant: true }]);

    const { container } = render(<Gates />);

    await waitFor(() => expect(screen.getByText("evidence ready")).toBeDefined());
    // Nothing to fix, so nothing to read.
    expect(container.textContent).not.toContain("needs ");
  });

  it("does not call an unanswerable check compliant", async () => {
    atGate([
      {
        bundle: "podtato-head-fa9",
        compliant: false,
        unknown: "no Fides trail for this artifact yet",
      },
    ]);

    render(<Gates />);

    // A Bundle whose evidence could not be checked is not a Bundle that passed,
    // and this is the direction a page would lie in if the two rendered alike.
    await waitFor(() => expect(screen.getByText("evidence unknown")).toBeDefined());
    expect(screen.getByText(/no Fides trail for this artifact yet/)).toBeDefined();
    expect(screen.queryByText("evidence ready")).toBeNull();
  });

  it("shows nothing at all when the Gate does not gate on evidence", async () => {
    atGate([]);

    const { container } = render(<Gates />);

    await waitFor(() => expect(screen.getByText(/Cross production/)).toBeDefined());
    // Most Gates do not, and a verdict column that is always blank is a column
    // people learn to ignore before the one Gate that uses it appears.
    expect(container.textContent).not.toContain("evidence ready");
    expect(container.textContent).not.toContain("would be refused");
    expect(container.textContent).not.toContain("evidence unknown");
  });
});
