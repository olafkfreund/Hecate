import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import Beacons from "@/app/beacons/page";
import { api, type Beacon } from "@/lib/api";

const beacon: Beacon = {
  metadata: { name: "podinfo", namespace: "uidemo" },
  spec: {
    interval: "5m",
    watch: [{ image: { repo: "ghcr.io/stefanprodan/podinfo", constraint: ">=6.0.0" } }],
  },
  status: {
    latestBundle: "podinfo-6b2",
    lastPolled: "2026-08-20T10:00:00Z",
    lastHandledReconcileAt: "earlier",
  },
};

function atList() {
  window.history.replaceState({}, "", "/beacons/?namespace=uidemo");
}

function atBeacon() {
  window.history.replaceState({}, "", "/beacons/?namespace=uidemo&name=podinfo");
}

describe("Beacons", () => {
  it("names each source the way its own ecosystem does", async () => {
    atList();
    vi.spyOn(api, "beacons").mockResolvedValue([
      beacon,
      {
        metadata: { name: "charts", namespace: "uidemo" },
        spec: { watch: [{ chart: { repo: "https://charts.example.com", name: "api" } }] },
      },
    ]);

    render(<Beacons />);

    await waitFor(() => expect(screen.getByText(/ghcr.io\/stefanprodan\/podinfo/)).toBeDefined());
    // A chart is identified by its name as well as its repo. Flattening both
    // kinds to "repo" would make a chart and its image look alike.
    expect(screen.getByText(/api https:\/\/charts.example.com/)).toBeDefined();
  });

  it("says when a Beacon is suspended", async () => {
    atList();
    vi.spyOn(api, "beacons").mockResolvedValue([
      { ...beacon, spec: { ...beacon.spec, suspend: true } },
    ]);

    render(<Beacons />);

    // Otherwise a suspended Beacon is indistinguishable from a quiet one, and
    // "nothing has shipped for a week" is usually this.
    await waitFor(() => expect(screen.getByText("suspended")).toBeDefined());
  });

  it("asks the named Beacon to look now", async () => {
    atBeacon();
    vi.spyOn(api, "beacon").mockResolvedValue(beacon);
    const poll = vi.spyOn(api, "poll").mockResolvedValue({ requestedAt: "now-token" });

    render(<Beacons />);

    await waitFor(() => expect(screen.getByText("Check for new versions")).toBeDefined());
    fireEvent.click(screen.getByText("Check for new versions"));

    await waitFor(() => expect(poll).toHaveBeenCalledWith("uidemo", "podinfo"));
  });

  it("distinguishes a queued check from one the controller has handled", async () => {
    atBeacon();
    // The Beacon comes back still reporting the token it had handled before,
    // so the request is queued rather than done.
    vi.spyOn(api, "beacon").mockResolvedValue(beacon);
    vi.spyOn(api, "poll").mockResolvedValue({ requestedAt: "now-token" });

    render(<Beacons />);

    await waitFor(() => expect(screen.getByText("Check for new versions")).toBeDefined());
    fireEvent.click(screen.getByText("Check for new versions"));

    await waitFor(() => expect(screen.getByText(/looks on its next turn/)).toBeDefined());
    expect(screen.queryByText("Checked.")).toBeNull();
  });

  it("warns that polling a suspended Beacon will not emit anything", async () => {
    atBeacon();
    vi.spyOn(api, "beacon").mockResolvedValue({
      ...beacon,
      spec: { ...beacon.spec, suspend: true },
    });

    render(<Beacons />);

    await waitFor(() =>
      expect(screen.getByText(/nothing will be emitted until it is resumed/)).toBeDefined(),
    );
  });
});
