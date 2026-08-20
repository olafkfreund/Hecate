import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Gates from "@/app/gates/page";
import Beacons from "@/app/beacons/page";
import Bundles from "@/app/bundles/page";
import Passages from "@/app/passages/page";
import { api, type Beacon, type Bundle, type Gate, type Passage } from "@/lib/api";

/**
 * What the list pages had stopped saying.
 *
 * Each of these was in the API response and not on the screen: the pages
 * rendered whatever fitted on one line of muted text, which turned out to be
 * about a third of what someone opens the page to find.
 */

function at(url: string) {
  window.history.replaceState({}, "", url);
}

const hour = 60 * 60 * 1000;
const recently = new Date(Date.now() - 2 * hour).toISOString();

describe("the Gates list", () => {
  const degraded: Gate = {
    metadata: { name: "production", namespace: "demo" },
    spec: {
      admits: [{ from: { beacon: "podtato-head" }, after: ["staging"], requireApproval: true }],
      suspend: false,
    },
    status: {
      current: { bundle: "podtato-head-fa9", since: recently },
      health: { status: "Degraded", issues: ["HelmRelease has not reconciled for 40m"] },
      eligible: ["podtato-head-fb1", "podtato-head-fc2"],
    },
  };

  it("says why a Gate is degraded, on the list", async () => {
    at("/gates/?namespace=demo");
    vi.spyOn(api, "gates").mockResolvedValue([degraded]);

    render(<Gates />);

    // A red dot and nothing else sends every reader to the same second page.
    await waitFor(() =>
      expect(screen.getByText(/HelmRelease has not reconciled/)).toBeDefined(),
    );
  });

  it("says how long the current Bundle has been there", async () => {
    at("/gates/?namespace=demo");
    vi.spyOn(api, "gates").mockResolvedValue([degraded]);

    const { container } = render(<Gates />);

    // A Gate holding the same Bundle for three weeks and one that took it a
    // minute ago look identical without this, and the difference is most of
    // what "is this current?" means.
    await waitFor(() => expect(container.textContent).toContain("2h ago"));
  });

  it("names what is waiting, and that a human has to say yes", async () => {
    at("/gates/?namespace=demo");
    vi.spyOn(api, "gates").mockResolvedValue([degraded]);

    const { container } = render(<Gates />);

    await waitFor(() => expect(container.textContent).toContain("2 waiting"));
    expect(container.textContent).toContain("podtato-head-fb1");
    expect(screen.getByText("needs approval")).toBeDefined();
    // Where it admits from was visible only as a label on an arrow.
    expect(container.textContent).toContain("after staging");
  });
});

describe("the Beacons list", () => {
  const beacon: Beacon = {
    metadata: { name: "podtato-head", namespace: "demo" },
    spec: {
      interval: "5m",
      watch: [{ image: { repo: "ghcr.io/podtato-head/podtato-server", constraint: "^0.3.0" } }],
    },
    status: { latestBundle: "podtato-head-fa9", lastPolled: recently },
  };

  it("says when it last looked and how often it looks", async () => {
    at("/beacons/?namespace=demo");
    vi.spyOn(api, "beacons").mockResolvedValue([beacon]);

    const { container } = render(<Beacons />);

    // "It is watching" is not the same as "it looked two hours ago", and a
    // Beacon that has quietly stopped polling looks fine without this.
    await waitFor(() => expect(container.textContent).toContain("looked 2h ago"));
    expect(container.textContent).toContain("every 5m");
    expect(screen.getByText("watching")).toBeDefined();
  });

  it("says when a Beacon is not ready, and why", async () => {
    at("/beacons/?namespace=demo");
    vi.spyOn(api, "beacons").mockResolvedValue([
      {
        ...beacon,
        status: {
          ...beacon.status,
          conditions: [
            { type: "Ready", status: "False", reason: "AuthFailed", message: "registry refused the credentials" },
          ],
        },
      },
    ]);

    render(<Beacons />);

    // A Beacon that cannot reach its registry looked exactly like one whose
    // registry has nothing new.
    await waitFor(() => expect(screen.getByText("not ready")).toBeDefined());
    expect(screen.getByText(/registry refused the credentials/)).toBeDefined();
  });
});

describe("the Bundles list", () => {
  it("says how far a Bundle has got, and where it stopped", async () => {
    at("/bundles/?namespace=demo");
    const bundle: Bundle = {
      metadata: { name: "podtato-head-fa9", namespace: "demo", creationTimestamp: recently },
      spec: { beacon: "podtato-head", artifacts: [{ image: { repo: "ghcr.io/x" } }] },
      status: {
        cleared: [{ gate: "staging", at: recently }],
        blocked: [{ gate: "production", at: recently, reason: "failing control: segregation-of-duties" }],
        approvedFor: [{ gate: "production", actor: "olaf@example.com", at: recently }],
      },
    };
    vi.spyOn(api, "bundles").mockResolvedValue([bundle]);

    const { container } = render(<Bundles />);

    // The question every Bundle row is opened to answer, and the one thing the
    // old row never said.
    await waitFor(() => expect(screen.getByText("in staging")).toBeDefined());
    expect(container.textContent).toContain("blocked at production");
    expect(screen.getByText(/segregation-of-duties/)).toBeDefined();
    expect(container.textContent).toContain("approved for production");
  });
});

describe("the Passages list", () => {
  it("says how far a failed Passage got before it failed", async () => {
    at("/passages/?namespace=demo");
    const passage: Passage = {
      metadata: { name: "podtato-head-fa9-production", namespace: "demo" },
      spec: { gate: "production", bundle: "podtato-head-fa9", actor: "olafkfreund" },
      status: {
        phase: "Failed",
        message: "evidence-gate: not compliant",
        startedAt: recently,
        finishedAt: new Date(Date.parse(recently) + 16000).toISOString(),
        steps: [
          { uses: "git-clone", phase: "Succeeded" },
          { uses: "evidence-gate", phase: "Failed" },
        ],
      },
    };
    vi.spyOn(api, "passages").mockResolvedValue([passage]);

    const { container } = render(<Passages />);

    // A Passage that failed on step one and one that failed on step nine are
    // different problems, and the list called both "Failed".
    await waitFor(() => expect(container.textContent).toContain("1/2 steps"));
    expect(container.textContent).toContain("16s");
    expect(container.textContent).toContain("olafkfreund");
    expect(screen.getByText("Failed")).toBeDefined();
  });
});
