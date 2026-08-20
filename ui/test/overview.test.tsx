import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import OverviewPage from "@/app/page";
import { api, type Day, type Overview, type Totals } from "@/lib/api";

const noTotals: Totals = {
  gates: 0,
  healthy: 0,
  progressing: 0,
  degraded: 0,
  unknown: 0,
  suspended: 0,
  eligible: 0,
  running: 0,
  failed: 0,
};

// Fourteen days, with today carrying the activity. The chart draws every day
// in the window including the empty ones, so a fixture with a short series
// would be testing a shape the server never sends.
const quiet: Day[] = Array.from({ length: 14 }, (_, i) => ({
  date: `2026-08-${String(7 + i).padStart(2, "0")}`,
  crossed: 0,
  failed: 0,
}));
const activity: Day[] = [...quiet.slice(0, 13), { date: "2026-08-20", crossed: 3, failed: 1 }];

const twoNamespaces: Overview = {
  activity,
  namespaces: [
    {
      namespace: "acme",
      gates: [
        {
          name: "production",
          health: "Degraded",
          issues: ["HelmRelease podinfo has not reconciled for 40m"],
          current: "podinfo-6a1",
          eligible: 2,
          suspended: false,
        },
        { name: "staging", health: "Healthy", current: "podinfo-6b2", eligible: 0, suspended: false },
      ],
    },
    {
      namespace: "team-b",
      gates: [
        {
          name: "production",
          health: "Progressing",
          current: "api-9",
          eligible: 0,
          running: "api-9-production-x1",
          suspended: false,
        },
      ],
    },
  ],
  totals: { ...noTotals, gates: 3, healthy: 1, progressing: 1, degraded: 1, eligible: 2, running: 1, failed: 1 },
};

describe("the overview", () => {
  it("says what needs attention before it says anything else", async () => {
    vi.spyOn(api, "overview").mockResolvedValue(twoNamespaces);

    const { container } = render(<OverviewPage />);
    // The count and its label are separate elements so the number can be
    // emphasised, so match on the rendered text rather than on one node.
    await waitFor(() => expect(container.textContent).toContain("degraded"));

    // The summary is the first thing on the page, ahead of any Gate. Someone
    // opening a dashboard is asking "is anything wrong", and a board that opens
    // with a table makes every reader do the summarising themselves.
    const text = container.textContent ?? "";
    expect(text.indexOf("degraded")).toBeLessThan(text.indexOf("acme"));
    expect(screen.getByText(/failed crossing/)).toBeDefined();
  });

  it("shows every namespace at once, which is the whole point", async () => {
    vi.spyOn(api, "overview").mockResolvedValue(twoNamespaces);

    render(<OverviewPage />);

    await waitFor(() => expect(screen.getByText("acme")).toBeDefined());
    expect(screen.getByText("team-b")).toBeDefined();
    // Two Gates named "production" in different namespaces are two Gates.
    expect(screen.getAllByText("production").length).toBe(2);
  });

  it("gives the reason for a bad health, not only the colour", async () => {
    vi.spyOn(api, "overview").mockResolvedValue(twoNamespaces);

    render(<OverviewPage />);

    // A red dot with no reason sends every reader to the same second page.
    await waitFor(() =>
      expect(screen.getByText(/HelmRelease podinfo has not reconciled/)).toBeDefined(),
    );
  });

  it("says so plainly when everything is healthy", async () => {
    vi.spyOn(api, "overview").mockResolvedValue({
      activity: quiet,
      namespaces: [
        {
          namespace: "acme",
          gates: [{ name: "production", health: "Healthy", eligible: 0, suspended: false }],
        },
      ],
      totals: { ...noTotals, gates: 1, healthy: 1 },
    });

    render(<OverviewPage />);

    // A sentence, not a card. "0 degraded" makes a reader check a number to
    // learn that nothing was wrong; saying so outright is one glance.
    await waitFor(() => expect(screen.getByText(/Nothing needs attention/)).toBeDefined());
    // No row of zeroes either. Six counts that are all zero bury the one that
    // is not, and a reader checks every one of them to learn nothing.
    expect(screen.queryByText("degraded")).toBeNull();
    expect(screen.queryByText("failed crossings")).toBeNull();
    expect(screen.queryByText("waiting to cross")).toBeNull();
    // The Gate count is always shown — it is the denominator for the rest.
    expect(screen.getByText("Gate")).toBeDefined();
  });

  it("reports a suspended Gate, which a health dot alone describes wrongly", async () => {
    vi.spyOn(api, "overview").mockResolvedValue({
      activity: quiet,
      namespaces: [
        {
          namespace: "acme",
          gates: [{ name: "production", health: "Healthy", eligible: 3, suspended: true }],
        },
      ],
      totals: { ...noTotals, gates: 1, healthy: 1, suspended: 1, eligible: 3 },
    });

    render(<OverviewPage />);
    await waitFor(() => expect(screen.getByText("production")).toBeDefined());

    // Twice: once in the summary count, and once on the Gate's own row. The
    // summary alone would say a Gate somewhere is suspended without saying
    // which, and asserting only "the word appears" would pass on that — it did,
    // until this counted.
    expect(screen.getAllByText("suspended").length).toBe(2);
  });

  it("explains an empty board as permissions rather than as nothing existing", async () => {
    vi.spyOn(api, "overview").mockResolvedValue({ namespaces: [], totals: noTotals, activity: quiet });

    render(<OverviewPage />);

    // The server filters to what the caller may read, so "no Gates" and "no
    // Gates you can see" are the same response — and the second is far more
    // likely to be why a new user's board is blank.
    await waitFor(() => expect(screen.getByText(/cannot see the namespaces/)).toBeDefined());
  });

  it("draws the crossings trend, and says so when there is nothing to draw", async () => {
    vi.spyOn(api, "overview").mockResolvedValue(twoNamespaces);
    const { container } = render(<OverviewPage />);

    await waitFor(() => expect(screen.getByText("Crossings")).toBeDefined());
    // recharts renders an SVG. jsdom gives it no size, so this asserts the
    // chart mounted rather than what it drew — the drawing is recharts' job
    // and is not worth a second implementation here.
    expect(container.querySelector(".recharts-responsive-container")).not.toBeNull();
    expect(screen.queryByText(/Nothing has crossed a Gate in the last/)).toBeNull();
  });

  it("says the trend is empty rather than drawing a flat line", async () => {
    vi.spyOn(api, "overview").mockResolvedValue({ ...twoNamespaces, activity: quiet });
    render(<OverviewPage />);

    // A chart of fourteen zeroes is a line along the axis, which reads as
    // "something went to nothing" rather than "nothing has happened".
    await waitFor(() =>
      expect(screen.getByText(/Nothing has crossed a Gate in the last 14 days/)).toBeDefined(),
    );
  });

  it("colours each Gate card by its health, and still prints the word", async () => {
    vi.spyOn(api, "overview").mockResolvedValue(twoNamespaces);
    const { container } = render(<OverviewPage />);

    await waitFor(() => expect(screen.getByText("acme")).toBeDefined());
    // The stripe carries the health at a glance...
    expect(container.innerHTML).toContain("border-l-[var(--destructive)]");
    expect(container.innerHTML).toContain("border-l-[var(--healthy)]");
    // ...and the word carries it for the roughly one man in twelve who cannot
    // reliably tell the green from the red.
    expect(screen.getByText("Degraded")).toBeDefined();
  });
});
