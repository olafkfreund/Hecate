import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Bundles from "@/app/bundles/page";
import { api, type Bundle } from "@/lib/api";

vi.mock("next/navigation", () => ({ usePathname: () => window.location.pathname }));

function bundle(status: Bundle["status"]): Bundle {
  return {
    metadata: { name: "podinfo-6b2", namespace: "demo" },
    spec: { beacon: "podinfo", alias: "wandering-owl" },
    status,
  };
}

const refusal = {
  gate: "production",
  at: "2026-08-14T12:24:16Z",
  reason: "evidence-gate: not compliant: Failing control: segregation-of-duties",
};

describe("a Bundle that was refused", () => {
  // "blocked at production" reads as a state that still holds. It is an event,
  // and one that may be ten days old — the Gate may admit the Bundle now, and
  // nothing on this card can say either way.
  it("says it was refused, and when, rather than that it is blocked", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([bundle({ blocked: [refusal] })]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText(/refused at production/)).toBeTruthy());
    expect(screen.queryByText(/blocked at/)).toBeNull();
  });

  it("shows the reason, which is the thing worth reading", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([bundle({ blocked: [refusal] })]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText(/segregation-of-duties/)).toBeTruthy());
  });

  // The bug this replaces: a Bundle refused on Monday and admitted on Tuesday
  // carried the red edge and the refusal for ever, because the card only asked
  // whether `blocked` was non-empty.
  it("forgets a refusal the Bundle has since overturned", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([
      bundle({
        blocked: [refusal],
        cleared: [{ gate: "production", at: "2026-08-20T09:00:00Z" }],
      }),
    ]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText(/in production/)).toBeTruthy());
    expect(screen.queryByText(/refused at/)).toBeNull();
    expect(screen.queryByText(/segregation-of-duties/)).toBeNull();
  });

  // Same Gate, and only the same Gate. Clearing staging says nothing about
  // whether production would admit it.
  it("keeps a refusal that a different Gate's crossing does not answer", async () => {
    vi.spyOn(api, "bundles").mockResolvedValue([
      bundle({
        blocked: [refusal],
        cleared: [{ gate: "staging", at: "2026-08-20T09:00:00Z" }],
      }),
    ]);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText(/refused at production/)).toBeTruthy());
  });
});
