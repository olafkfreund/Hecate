import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import Passages from "@/app/passages/page";
import { api, ApiError, type Passage } from "@/lib/api";

const running: Passage = {
  metadata: { name: "podinfo-6b2-production-xk4", namespace: "uidemo" },
  spec: { gate: "production", bundle: "podinfo-6b2", actor: "ada@example.com" },
  status: {
    phase: "Running",
    startedAt: "2026-08-20T10:00:00Z",
    currentStep: 1,
    steps: [
      {
        uses: "git-clone",
        phase: "Succeeded",
        startedAt: "2026-08-20T10:00:00Z",
        finishedAt: "2026-08-20T10:00:04Z",
        // Ran once, like most steps do. Present so the boundary is exercised:
        // the page must not label this, or every step carries an "attempts"
        // and the one that retried seven times stops standing out.
        attempts: 1,
      },
      // Started but not finished — the case a duration must NOT be invented
      // for. Without startedAt here the "still running" branch is never
      // reached and an assertion about it proves nothing.
      {
        uses: "flux-wait",
        as: "settle",
        phase: "Running",
        startedAt: "2026-08-20T10:00:04Z",
        attempts: 7,
      },
    ],
  },
};

function atPassage() {
  window.history.replaceState(
    {},
    "",
    "/passages/?namespace=uidemo&name=podinfo-6b2-production-xk4",
  );
}

describe("one Passage", () => {
  it("shows each step, its duration, and how many attempts it took", async () => {
    atPassage();
    vi.spyOn(api, "passage").mockResolvedValue(running);

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("git-clone")).toBeDefined());
    // A finished step has a duration. Four seconds, from the two timestamps.
    expect(screen.getByText("4s")).toBeDefined();
    // A step that waits on an external system is invoked repeatedly, and the
    // count is the difference between "slow" and "stuck in a loop".
    expect(screen.getByText("7 attempts")).toBeDefined();
    // A step that ran once says nothing. Labelling every step would bury the
    // one above in noise, which is the only reason the count is worth showing.
    expect(screen.queryByText("1 attempts")).toBeNull();
    // Neither the running step nor the running Passage gets a duration:
    // computed once at render it would freeze, and a frozen clock reads as a
    // wedged step rather than a stale number.
    expect(document.body.textContent).not.toContain("0s");
  });

  it("does not offer to abort a Passage that has already finished", async () => {
    atPassage();
    vi.spyOn(api, "passage").mockResolvedValue({
      ...running,
      status: { ...running.status, phase: "Succeeded", finishedAt: "2026-08-20T10:01:00Z" },
    });

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("git-clone")).toBeDefined());
    expect(screen.queryByText("Abort this Passage")).toBeNull();
  });

  it("aborts the Passage it is showing, and reloads", async () => {
    atPassage();
    const passage = vi.spyOn(api, "passage").mockResolvedValue(running);
    const abort = vi.spyOn(api, "abort").mockResolvedValue({
      passage: "podinfo-6b2-production-xk4",
      aborted: true,
      abortedBy: "ada@example.com",
    });

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("Abort this Passage")).toBeDefined());
    fireEvent.click(screen.getByText("Abort this Passage"));

    await waitFor(() =>
      expect(abort).toHaveBeenCalledWith("uidemo", "podinfo-6b2-production-xk4"),
    );
    // Reloaded, so the phase on screen is the one after the abort rather than
    // the one that made the button appear.
    await waitFor(() => expect(passage).toHaveBeenCalledTimes(2));
  });

  it("explains a refused abort as a separate permission, not a fault", async () => {
    atPassage();
    vi.spyOn(api, "passage").mockResolvedValue(running);
    vi.spyOn(api, "abort").mockRejectedValue(new ApiError(403, "forbidden"));

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("Abort this Passage")).toBeDefined());
    fireEvent.click(screen.getByText("Abort this Passage"));

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("separate permission"),
    );
  });

  it("says an abort is already under way rather than offering it twice", async () => {
    atPassage();
    vi.spyOn(api, "passage").mockResolvedValue({
      ...running,
      spec: { ...running.spec, abort: true },
    });

    render(<Passages />);

    await waitFor(() =>
      expect(screen.getByText(/An abort has been requested/)).toBeDefined(),
    );
    // The controller stops at a step boundary, so there is a window where the
    // Passage is still Running. A second button there would do nothing.
    expect(screen.queryByText("Abort this Passage")).toBeNull();
  });
});
