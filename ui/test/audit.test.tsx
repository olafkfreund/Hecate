import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import AuditPage from "@/app/audit/page";
import { api, ApiError, Unauthenticated, type AuditEntry } from "@/lib/api";

const entries: AuditEntry[] = [
  {
    at: "2026-08-20T10:00:00Z",
    kind: "refused",
    gate: "production",
    bundle: "podinfo-6b2",
    detail: "change gate held: risk 71",
    evidence: { verdict: "hold", risk: 71, blockers: ["no approved change request"] },
  },
  {
    at: "2026-08-20T09:00:00Z",
    kind: "crossed",
    gate: "staging",
    bundle: "podinfo-6b2",
    actor: "ada@example.com",
    verified: true,
    digest: "sha256:abcdef0123456789",
  },
];

function atNamespace() {
  window.history.replaceState({}, "", "/audit/?namespace=uidemo");
}

describe("the audit trail", () => {
  it("shows refusals as prominently as crossings", async () => {
    atNamespace();
    vi.spyOn(api, "audit").mockResolvedValue(entries);

    render(<AuditPage />);

    await waitFor(() => expect(screen.getByText("refused")).toBeDefined());
    // A page listing only what shipped is a deployment log. What makes this an
    // audit trail is that it also holds what was stopped, and by what.
    expect(screen.getByText("crossed")).toBeDefined();
    expect(screen.getByText(/change gate held/)).toBeDefined();
    expect(screen.getByText(/no approved change request/)).toBeDefined();
  });

  it("takes every colour from the theme rather than a literal", async () => {
    atNamespace();
    vi.spyOn(api, "audit").mockResolvedValue(entries);

    const { container } = render(<AuditPage />);
    await waitFor(() => expect(screen.getByText("refused")).toBeDefined());

    // globals.css copies Fides' palette whole so the two products look like one
    // platform. A literal `text-red-500` answers to neither the theme nor the
    // other product, and reads wrong in dark mode — which is what this page had.
    const literal = /\b(text|bg|border)-(red|green|amber|blue|yellow|orange)-\d{3}\b/;
    expect(container.innerHTML).not.toMatch(literal);
    // And the tokens are actually reaching the markup.
    expect(container.innerHTML).toContain("var(--destructive)");
    expect(container.innerHTML).toContain("var(--healthy)");
  });

  it("offers a way back in when the session has expired", async () => {
    atNamespace();
    vi.spyOn(api, "audit").mockRejectedValue(new Unauthenticated());

    render(<AuditPage />);

    // The button, not the heading — both say "Sign in", and it is the button
    // that is the way back in. This page used to load by hand and render the
    // raw error, leaving someone whose session merely expired with nowhere to
    // go, on the page an auditor is most likely to arrive at cold.
    await waitFor(() => expect(screen.getByRole("button", { name: /Sign in/ })).toBeDefined());
  });

  it("explains a forbidden namespace as the cluster's decision", async () => {
    atNamespace();
    vi.spyOn(api, "audit").mockRejectedValue(new ApiError(403, "forbidden"));

    render(<AuditPage />);

    await waitFor(() => expect(screen.getByText("Not permitted")).toBeDefined());
    expect(screen.getByText(/Hecate does not decide this; the cluster does/)).toBeDefined();
  });

  it("says an automatic crossing was automatic rather than leaving it blank", async () => {
    atNamespace();
    vi.spyOn(api, "audit").mockResolvedValue([{ ...entries[1], actor: undefined }]);

    render(<AuditPage />);

    await waitFor(() => expect(screen.getByText(/automatically/)).toBeDefined());
  });
});
