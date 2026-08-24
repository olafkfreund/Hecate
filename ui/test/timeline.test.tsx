import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Bundles from "@/app/bundles/page";
import { timeline, took } from "@/lib/timeline";
import { api } from "@/lib/api";
import * as fixtures from "./fixtures";

describe("the Bundle timeline", () => {
  it("puts events in the order they happened, not the order they are stored", () => {
    const events = timeline(fixtures.travelled);

    // cleared and blocked are separate lists on the Bundle; concatenating them
    // would put a 09:05 event before a 09:02 one and read as cause after
    // effect.
    expect(events.map((e) => e.kind)).toEqual(["emitted", "blocked", "cleared"]);
    const times = events.map((e) => Date.parse(e.at));
    expect([...times].sort((a, b) => a - b)).toEqual(times);
  });

  it("starts at the emission, which no crossing records", () => {
    const events = timeline(fixtures.travelled);
    expect(events[0].kind).toBe("emitted");
    expect(events[0].at).toBe("2026-08-10T09:00:00Z");
  });

  it("survives a Bundle with no history at all", () => {
    expect(() => timeline(fixtures.bundles[0])).not.toThrow();
    expect(timeline(fixtures.bundles[0])).toEqual([]);
  });
});

describe("the Bundle detail page", () => {
  it("shows what it is, who moved it and why it was refused", async () => {
    window.history.replaceState({}, "", "/bundles/?name=podinfo-6b2&namespace=uidemo");
    vi.spyOn(api, "bundle").mockResolvedValue(fixtures.travelled);

    render(<Bundles />);

    await waitFor(() => expect(screen.getByText("wandering-owl")).toBeDefined());

    // The artifact, with the digest — a tag can be moved, a digest cannot, and
    // the digest is what was actually deployed.
    expect(screen.getByText("ghcr.io/stefanprodan/podinfo:6.14.1")).toBeDefined();
    expect(screen.getByText(/sha256:4a6f31e7/)).toBeDefined();

    // The audit trail: who, and why not.
    //
    // Exact matches, not substrings: the event "cleared staging" and the
    // refusal reason "has not cleared staging" both contain the same words, so
    // a loose matcher finds two elements and proves neither. That collision is
    // in the rendered page too, which is worth knowing.
    expect(screen.getByText("cleared staging")).toBeDefined();
    expect(screen.getByText("did not clear production")).toBeDefined();
    expect(screen.getByText("has not cleared staging")).toBeDefined();
    expect(screen.getByText(/by controller/)).toBeDefined();
    expect(screen.getByText(/by olaf@hecate.test/)).toBeDefined();
  });

  // approvedFor used to be a list of Gate names. Rendering it with .join
  // produced "[object Object]" the moment it grew an approver — the exact
  // failure a hand-written type sharing its author's assumptions cannot catch,
  // which is why the fixture is checked against what the server marshals.
  it("names the approver, not just the Gate", async () => {
    window.history.replaceState({}, "", "/bundles/?name=podinfo-6b2&namespace=uidemo");
    vi.spyOn(api, "bundle").mockResolvedValue(fixtures.travelled);

    const { container } = render(<Bundles />);

    await waitFor(() => expect(screen.getByText("wandering-owl")).toBeDefined());
    expect(container.textContent).toContain("Approved for");
    expect(container.textContent).toContain("olaf@acme.example");
    expect(container.textContent).not.toContain("object Object");
  });

  it("is the list when no Bundle is named", async () => {
    window.history.replaceState({}, "", "/bundles/");
    const list = vi.spyOn(api, "bundles").mockResolvedValue(fixtures.bundles);

    render(<Bundles />);

    await waitFor(() => expect(list).toHaveBeenCalledWith());
  });
});

describe("how long something took", () => {
  it("rounds to the unit a person would say it in", () => {
    const from = "2026-08-20T10:00:00Z";
    expect(took(from, "2026-08-20T10:00:04Z")).toBe("4s");
    expect(took(from, "2026-08-20T10:02:00Z")).toBe("2m");
    expect(took(from, "2026-08-20T10:02:30Z")).toBe("2m 30s");
    expect(took(from, "2026-08-20T11:00:00Z")).toBe("1h");
    expect(took(from, "2026-08-20T11:30:00Z")).toBe("1h 30m");
  });

  it("says nothing rather than guessing", () => {
    // Still running: no end, so no duration. See took's own note — a value
    // computed once at render freezes and reads as a wedged step.
    expect(took("2026-08-20T10:00:00Z", undefined)).toBeNull();
    expect(took(undefined, "2026-08-20T10:00:00Z")).toBeNull();
    // Clocks disagree across nodes, and a negative duration is a wrong answer
    // rather than a small one.
    expect(took("2026-08-20T10:00:05Z", "2026-08-20T10:00:00Z")).toBeNull();
    expect(took("not a time", "also not")).toBeNull();
  });
});
