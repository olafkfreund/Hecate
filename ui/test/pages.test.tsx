import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Gates from "@/app/page";
import Bundles from "@/app/bundles/page";
import Passages from "@/app/passages/page";
import GateDetail from "@/app/gates/page";
import { api, Unauthenticated } from "@/lib/api";
import * as fixtures from "./fixtures";

/**
 * These exist because three UI changes shipped without the authenticated page
 * ever being rendered, and two of them were broken in ways nothing else could
 * have caught: a type that did not match the server's JSON, and a hook seeded
 * from the wrong snapshot. The unauthenticated path was easy to check in a
 * browser; the one with data was not, and that is precisely the one that broke.
 */

function atNamespace(query: string) {
  window.history.replaceState({}, "", query);
}

describe("pages render the data the API actually returns", () => {
  it("Gates lists every Gate, its health and what may cross", async () => {
    atNamespace("/?namespace=uidemo");
    vi.spyOn(api, "gates").mockResolvedValue(fixtures.gates);

    render(<Gates />);

    // getAllByText, because each Gate appears twice: once as a node in the
    // pipeline graph and once as a row in the table. Asserting on exactly one
    // would break the moment the graph renders, which is not a regression.
    await waitFor(() => expect(screen.getAllByText("production").length).toBe(2));
    expect(screen.getAllByText("staging").length).toBe(2);
    // The eligible Bundle is the actionable fact on this page.
    expect(screen.getAllByText(/podinfo-6b2/).length).toBeGreaterThan(0);
    expect(screen.getAllByText("NotApplicable").length).toBe(2);
  });

  it("draws the pipeline the Gates imply, including where approval is needed", async () => {
    atNamespace("/?namespace=uidemo");
    vi.spyOn(api, "gates").mockResolvedValue(fixtures.gates);

    render(<Gates />);

    // The graph is derived from spec.admits, so the Beacon appears even though
    // no Beacon was fetched — that is the whole point of deriving it.
    await waitFor(() => expect(screen.getByText("podinfo")).toBeDefined());
    expect(screen.getByText("beacon")).toBeDefined();
    // production admits `after: [staging]` with requireApproval, so the edge
    // between them has to say so.
    expect(screen.getByText("approval")).toBeDefined();
    expect(screen.getByRole("img", { name: /Pipeline: 1 beacon feeding 2 gates/ })).toBeDefined();
  });

  it("Bundles prefers the alias, which is what a person recognises", async () => {
    atNamespace("/?namespace=uidemo");
    vi.spyOn(api, "bundles").mockResolvedValue(fixtures.bundles);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText("wandering-owl")).toBeDefined());
  });

  it("Passages shows where each one was going and how it ended", async () => {
    atNamespace("/?namespace=uidemo");
    vi.spyOn(api, "passages").mockResolvedValue(fixtures.passages);

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("podinfo-6b2")).toBeDefined());
    expect(screen.getByText(/staging/)).toBeDefined();
    expect(screen.getByText("Succeeded")).toBeDefined();
  });

  it("the Gate detail shows blockers with the fix, not just the diagnosis", async () => {
    atNamespace("/gates/?name=staging&namespace=uidemo");
    vi.spyOn(api, "explain").mockResolvedValue(fixtures.explanation);

    render(<GateDetail />);

    await waitFor(() => expect(screen.getByText("staging")).toBeDefined());
    expect(screen.getByText("AwaitingRequest")).toBeDefined();
    expect(screen.getByText(/spec.auto is false/)).toBeDefined();
    // A diagnosis nobody can act on is just a status.
    expect(screen.getByText(/hecate promote staging/)).toBeDefined();
  });

  it("shows the change gate's verdict and risk score even when it approved", async () => {
    atNamespace("/gates/?name=staging&namespace=uidemo");
    vi.spyOn(api, "explain").mockResolvedValue({
      ...fixtures.explanation,
      state: "Crossing",
      evidence: { trail: "t-1", verdict: "approve", risk: 12 },
    });

    const { container } = render(<GateDetail />);

    await waitFor(() => expect(screen.getByText("staging")).toBeDefined());
    // An approved verdict produces no blocker, so without this the score is
    // visible only when something is already wrong.
    expect(container.textContent).toContain("approve");
    expect(container.textContent).toContain("risk 12/100");
  });
});

describe("the states that are not data", () => {
  it("offers a sign-in rather than an error when the session has gone", async () => {
    atNamespace("/?namespace=uidemo");
    vi.spyOn(api, "gates").mockRejectedValue(new Unauthenticated());

    render(<Gates />);

    await waitFor(() => expect(screen.getByRole("button", { name: /sign in/i })).toBeDefined());
  });

  it("says an empty namespace is empty, rather than showing nothing at all", async () => {
    atNamespace("/?namespace=empty");
    vi.spyOn(api, "gates").mockResolvedValue([]);

    render(<Gates />);

    await waitFor(() => expect(screen.getByText(/No Gates in/)).toBeDefined());
  });

  it("reads the namespace from the URL, not from a hydration-time default", async () => {
    atNamespace("/?namespace=uidemo");
    const gates = vi.spyOn(api, "gates").mockResolvedValue(fixtures.gates);

    render(<Gates />);

    // The bug that shipped: the namespace was frozen at the fallback, so the
    // page queried "default" while the URL said otherwise.
    await waitFor(() => expect(gates).toHaveBeenCalledWith("uidemo"));
  });
});
