import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import Approvals from "@/app/approvals/page";
import { api, ApiError, type Explanation } from "@/lib/api";
import * as fixtures from "./fixtures";

const awaitingApproval: Explanation = {
  gate: "production",
  namespace: "uidemo",
  state: "Blocked",
  summary: "1 Bundle cannot cross: awaiting approval",
  waiting: [{ bundle: "podinfo-6b2", reason: "awaiting approval", kind: "AwaitingApproval" }],
};

const waitingOnUpstream: Explanation = {
  gate: "staging",
  namespace: "uidemo",
  state: "Blocked",
  summary: "1 Bundle cannot cross",
  waiting: [{ bundle: "podinfo-6b2", reason: "has not cleared dev", kind: "UpstreamNotCleared" }],
};

function atNamespace() {
  window.history.replaceState({}, "", "/approvals/?namespace=uidemo");
}

describe("the approval queue", () => {
  it("lists only what a human can actually unblock", async () => {
    atNamespace();
    vi.spyOn(api, "gates").mockResolvedValue(fixtures.gates);
    vi.spyOn(api, "explain").mockImplementation(async (_ns, name) =>
      name === "production" ? awaitingApproval : waitingOnUpstream,
    );

    render(<Approvals />);

    await waitFor(() => expect(screen.getByText("podinfo-6b2")).toBeDefined());
    // The staging entry is waiting on an upstream Gate. Offering to approve it
    // would offer an action that cannot help.
    expect(screen.getByText("→ production")).toBeDefined();
    expect(screen.queryByText("→ staging")).toBeNull();
  });

  it("filters on the code, not the wording of the reason", async () => {
    atNamespace();
    vi.spyOn(api, "gates").mockResolvedValue([fixtures.gates[0]]);
    vi.spyOn(api, "explain").mockResolvedValue({
      ...awaitingApproval,
      // Same code, completely different prose. A UI matching on the message
      // would drop this, and the message is not the contract.
      waiting: [{ bundle: "podinfo-6b2", reason: "somebody has to say yes", kind: "AwaitingApproval" }],
    });

    render(<Approvals />);

    await waitFor(() => expect(screen.getByText("podinfo-6b2")).toBeDefined());
  });

  it("approves for the named Gate, and refreshes", async () => {
    atNamespace();
    vi.spyOn(api, "gates").mockResolvedValue([fixtures.gates[0]]);
    vi.spyOn(api, "explain").mockResolvedValue(awaitingApproval);
    const approve = vi.spyOn(api, "approve").mockResolvedValue({});

    render(<Approvals />);
    await waitFor(() => expect(screen.getByRole("button", { name: /Approve for production/ })).toBeDefined());
    fireEvent.click(screen.getByRole("button", { name: /Approve for production/ }));

    // Bundle first, then Gate — swapping them would approve the wrong thing
    // and the API would accept it.
    await waitFor(() => expect(approve).toHaveBeenCalledWith("uidemo", "podinfo-6b2", "production"));
  });

  it("explains a refusal as the control working, not a fault", async () => {
    atNamespace();
    vi.spyOn(api, "gates").mockResolvedValue([fixtures.gates[0]]);
    vi.spyOn(api, "explain").mockResolvedValue(awaitingApproval);
    vi.spyOn(api, "approve").mockRejectedValue(new ApiError(403, "may not update bundles/status"));

    render(<Approvals />);
    await waitFor(() => expect(screen.getByRole("button", { name: /Approve for/ })).toBeDefined());
    fireEvent.click(screen.getByRole("button", { name: /Approve for/ }));

    await waitFor(() => expect(screen.getByRole("alert")).toBeDefined());
    // Someone told "forbidden" files a ticket. Someone told the separation is
    // deliberate goes and finds the second approver.
    expect(screen.getByRole("alert").textContent).toMatch(/separate permission/);
  });

  it("says so when nothing is waiting", async () => {
    atNamespace();
    vi.spyOn(api, "gates").mockResolvedValue([fixtures.gates[1]]);
    vi.spyOn(api, "explain").mockResolvedValue(waitingOnUpstream);

    render(<Approvals />);

    await waitFor(() => expect(screen.getByText(/Nothing is waiting on you/)).toBeDefined());
  });
});
