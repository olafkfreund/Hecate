import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import { Shell } from "@/components/shell";
import Gates from "@/app/gates/page";
import Bundles from "@/app/bundles/page";
import { api, type Bundle, type Gate } from "@/lib/api";

vi.mock("next/navigation", () => ({ usePathname: () => window.location.pathname }));

/**
 * These replace the namespace picker's tests, and the replacement is the point.
 *
 * A picker made "what is happening" a question you could only ask about a place
 * you had already guessed: every namespace but one was hidden behind a control
 * most people never touched, and the page said "nothing here" when the honest
 * answer was "nothing in the one namespace you happen to be pointed at".
 *
 * Everything is shown now, so what is worth testing is the placement — that the
 * namespaces are all there, told apart, and not announced when there is only
 * one of them to tell apart from.
 */

function gateIn(namespace: string, name: string): Gate {
  return {
    metadata: { name, namespace },
    spec: { admits: [{ from: { beacon: "podinfo" } }] },
    status: { health: { status: "NotApplicable" } },
  };
}

function bundleIn(namespace: string, name: string): Bundle {
  return { metadata: { name, namespace }, spec: { beacon: "podinfo" }, status: {} };
}

describe("showing every namespace", () => {
  it("lists Gates from more than one namespace at once", async () => {
    vi.spyOn(api, "gates").mockResolvedValue([
      gateIn("team-a", "production"),
      gateIn("team-b", "staging"),
    ]);

    render(<Gates />);

    // Both Gates, without anyone choosing a namespace first. getAllByText
    // because each namespace draws its own pipeline diagram, so a Gate's name
    // appears on its card and again in the graph above it.
    await waitFor(() => expect(screen.getAllByText("production").length).toBeGreaterThan(0));
    expect(screen.getAllByText("staging").length).toBeGreaterThan(0);
    // And the namespaces named, because two Gates with different names in
    // different namespaces are otherwise indistinguishable from two in one.
    expect(screen.getByText("team-a")).toBeTruthy();
    expect(screen.getByText("team-b")).toBeTruthy();
  });

  it("asks the API once, not once per namespace", async () => {
    const gates = vi.spyOn(api, "gates").mockResolvedValue([
      gateIn("team-a", "production"),
      gateIn("team-b", "staging"),
    ]);

    render(<Gates />);

    await waitFor(() => expect(screen.getAllByText("production").length).toBeGreaterThan(0));
    // The whole reason these routes exist: a request per namespace would make
    // the page slower the more of the cluster you are trusted with.
    expect(gates).toHaveBeenCalledTimes(1);
    expect(gates).toHaveBeenCalledWith();
  });

  it("does not head a single namespace, which would be chrome and no information", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([bundleIn("only-one", "podinfo-6b2")]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText(/podinfo-6b2|wandering/)).toBeTruthy());
    expect(screen.queryByText("only-one")).toBeNull();
  });

  it("heads each namespace when there is more than one", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([
      bundleIn("team-a", "podinfo-6b2"),
      bundleIn("team-b", "podinfo-7c3"),
    ]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText("team-a")).toBeTruthy());
    expect(screen.getByText("team-b")).toBeTruthy();
  });
});

describe("what the shell no longer offers", () => {
  // The control is gone rather than hidden. A picker that still existed
  // somewhere would be a second way to narrow the page, disagreeing with the
  // one thing every screen now promises: you are seeing everything you can see.
  it("has no namespace picker on any page", async () => {
    window.history.replaceState({}, "", "/gates/");

    render(
      <Shell>
        <p>anything</p>
      </Shell>,
    );

    await waitFor(() => expect(screen.getByText("anything")).toBeTruthy());
    expect(screen.queryByLabelText("Namespace")).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
  });
});
