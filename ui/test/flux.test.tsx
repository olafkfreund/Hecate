import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { FluxPanel } from "@/components/flux";
import { api, ApiError, type FluxResource } from "@/lib/api";

const healthy: FluxResource = {
  kind: "Kustomization",
  name: "podinfo",
  namespace: "demo",
  suspended: false,
  health: "Healthy",
  revision: "main@sha1:abc",
  missing: false,
};

const suspended: FluxResource = { ...healthy, suspended: true };

describe("the Flux panel", () => {
  it("keeps a banner up for as long as anything is suspended", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([suspended]);

    render(<FluxPanel namespace="demo" gate="production" />);

    // Not a toast when it happens and not a badge on the card. A suspension
    // outlives the session that made it: someone pauses production to debug, is
    // interrupted, and every crossing afterwards reports success while changing
    // nothing.
    await waitFor(() => expect(screen.getByRole("status")).toBeDefined());
    expect(screen.getByRole("status").textContent).toContain("Kustomization podinfo is suspended");
    expect(screen.getByRole("status").textContent).toContain("report success and change nothing");
    // And says it will not go away on its own.
    expect(screen.getByRole("status").textContent).toContain("not in git");
  });

  it("has no banner when nothing is suspended", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([healthy]);

    render(<FluxPanel namespace="demo" gate="production" />);

    await waitFor(() => expect(screen.getByText(/Kustomization podinfo/)).toBeDefined());
    // A warning that is always present is one nobody reads.
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("offers Resume for something suspended, and Suspend for something running", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([suspended]);
    const { rerender } = render(<FluxPanel namespace="demo" gate="production" />);
    await waitFor(() => expect(screen.getByText("Resume")).toBeDefined());
    expect(screen.queryByText("Suspend")).toBeNull();

    vi.spyOn(api, "flux").mockResolvedValue([healthy]);
    rerender(<FluxPanel namespace="demo" gate="staging" />);
    await waitFor(() => expect(screen.getByText("Suspend")).toBeDefined());
  });

  it("will not offer to reconcile something that is suspended", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([suspended]);

    render(<FluxPanel namespace="demo" gate="production" />);

    // Flux ignores a reconcile request on a suspended resource, so the button
    // would appear to work and do nothing — which is the same failure the
    // banner exists to prevent, in miniature.
    const button = await screen.findByText("Reconcile now");
    expect((button.closest("button") as HTMLButtonElement).disabled).toBe(true);
  });

  it("asks the server to suspend the resource it is showing", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([healthy]);
    const suspend = vi.spyOn(api, "suspendFlux").mockResolvedValue({
      kind: "Kustomization",
      name: "podinfo",
      suspended: true,
      by: "ada",
    });

    render(<FluxPanel namespace="demo" gate="production" />);
    fireEvent.click(await screen.findByText("Suspend"));

    await waitFor(() =>
      expect(suspend).toHaveBeenCalledWith("demo", "production", "Kustomization", "podinfo", true),
    );
  });

  it("explains a refusal as a separate permission, and names the role", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([healthy]);
    vi.spyOn(api, "suspendFlux").mockRejectedValue(new ApiError(403, "forbidden"));

    render(<FluxPanel namespace="demo" gate="production" />);
    fireEvent.click(await screen.findByText("Suspend"));

    // The button stays rather than vanishing: a control that disappears teaches
    // nobody that the right exists or who to ask for it.
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("separate permission"),
    );
    expect(screen.getByRole("alert").textContent).toContain("hecate-flux-operator");
  });

  it("distinguishes a queued reconcile from one Flux has handled", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([healthy]);
    vi.spyOn(api, "reconcileFlux").mockResolvedValue({ requestedAt: "stamp-1" });

    render(<FluxPanel namespace="demo" gate="production" />);
    fireEvent.click(await screen.findByText("Reconcile now"));

    // lastHandled is still empty, so the request is queued rather than done.
    await waitFor(() => expect(screen.getByText(/acts on its next turn/)).toBeDefined());
    expect(screen.queryByText("Reconciled.")).toBeNull();
  });

  it("offers no buttons for a resource that is not in the cluster", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([
      { ...healthy, missing: true, detail: "not in the cluster" },
    ]);

    render(<FluxPanel namespace="demo" gate="production" />);

    // Twice — the pill and the detail line both say it. getAllByText, because
    // the assertion is about the buttons below, not about how many places say
    // the resource is absent.
    await waitFor(() => expect(screen.getAllByText("not in the cluster").length).toBeGreaterThan(0));
    // Suspending something that does not exist is not an operation.
    expect(screen.queryByText("Suspend")).toBeNull();
    expect(screen.queryByText("Reconcile now")).toBeNull();
  });

  it("shows nothing at all when the Gate watches no Flux resources", async () => {
    vi.spyOn(api, "flux").mockResolvedValue([]);

    const { container } = render(<FluxPanel namespace="demo" gate="production" />);

    await waitFor(() => expect(container.textContent).not.toContain("Loading"));
    // An empty "Flux" heading on a Gate that has nothing to do with Flux is a
    // section someone has to read to learn it does not apply.
    expect(container.textContent).not.toContain("Flux");
  });
});
