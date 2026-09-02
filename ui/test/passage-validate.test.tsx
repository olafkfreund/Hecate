import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import NewPassage from "@/app/passages/new/page";
import { api, type StepSchema } from "@/lib/api";
import * as fixtures from "./fixtures";

/**
 * The point of hecate#172 scope item 4: the form calls the same
 * `Registry.Validate` the admission path runs, and attaches each problem to
 * the step row it names — not a wall of text at the top of the page.
 */

const catalogue: Record<string, StepSchema> = {
  "git-commit": {
    type: "object",
    description: "Commit staged changes.",
    properties: { message: { type: "string", description: "Commit message." } },
  },
  "git-clone": {
    type: "object",
    description: "Clone a repository.",
    properties: { repo: { type: "string", description: "Clone URL." } },
  },
};

function stubCatalogue() {
  vi.spyOn(api, "stepSchemas").mockResolvedValue(catalogue);
  vi.spyOn(api, "gates").mockResolvedValue(fixtures.gates);
  vi.spyOn(api, "bundles").mockResolvedValue(fixtures.bundles);
}

describe("live step-list validation", () => {
  it("attaches a problem to the step row it names, not the page as a whole", async () => {
    stubCatalogue();
    const validate = vi.spyOn(api, "validatePassage").mockResolvedValue({
      problems: [{ index: 0, uses: "git-commit", message: "message is required" }],
    });

    render(<NewPassage />);

    await waitFor(() => expect(screen.getByText("git-commit")).toBeDefined());
    fireEvent.click(screen.getAllByRole("button", { name: /git-commit/ })[0]);

    await waitFor(() => expect(validate).toHaveBeenCalled(), { timeout: 2000 });
    await waitFor(
      () => expect(screen.getByRole("alert")).toBeDefined(),
      { timeout: 2000 },
    );
    expect(screen.getByRole("alert").textContent).toBe("message is required");

    // What was actually sent is the composed step list — the same shape
    // authorPassage takes — not a reimplementation of the rule in the browser.
    expect(validate).toHaveBeenCalledWith([{ uses: "git-commit", as: undefined, with: {} }]);
  });

  it("places a problem on its own row when several steps are listed", async () => {
    stubCatalogue();
    vi.spyOn(api, "validatePassage").mockResolvedValue({
      problems: [{ index: 1, uses: "git-clone", message: "repo is required" }],
    });

    render(<NewPassage />);

    await waitFor(() => expect(screen.getByText("git-commit")).toBeDefined());
    fireEvent.click(screen.getAllByRole("button", { name: /git-commit/ })[0]);
    fireEvent.click(screen.getAllByRole("button", { name: /git-clone/ })[0]);

    await waitFor(() => expect(screen.getByRole("alert")).toBeDefined(), { timeout: 2000 });

    const alert = screen.getByRole("alert");
    expect(alert.textContent).toBe("repo is required");

    // The message sits under step #2 (git-clone), not step #1 (git-commit).
    const row = alert.closest("li");
    expect(row?.textContent).toContain("git-clone");
    expect(row?.textContent).not.toContain("git-commit");
  });

  it("clears its problems when the step list empties, without a fresh call", async () => {
    stubCatalogue();
    vi.spyOn(api, "validatePassage").mockResolvedValue({
      problems: [{ index: 0, uses: "git-commit", message: "message is required" }],
    });

    render(<NewPassage />);

    await waitFor(() => expect(screen.getByText("git-commit")).toBeDefined());
    fireEvent.click(screen.getAllByRole("button", { name: /git-commit/ })[0]);
    await waitFor(() => expect(screen.getByRole("alert")).toBeDefined(), { timeout: 2000 });

    fireEvent.click(screen.getByRole("button", { name: /Remove step 1/ }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });
});
