import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Settings from "@/app/settings/page";
import { api, type Settings as SettingsData } from "@/lib/api";

const base: SettingsData = {
  version: "0.4.0",
  identity: { name: "promoter@hecate.demo" },
  fides: [],
  clusters: [],
  telemetry: { configured: false },
  home: { inCluster: true, server: "https://10.0.0.1:443", namespace: "hecate-system" },
};

function show(settings: SettingsData) {
  window.history.replaceState({}, "", "/settings/?namespace=demo");
  vi.spyOn(api, "settings").mockResolvedValue(settings);
  vi.spyOn(api, "grants").mockResolvedValue({ grants: [] });
  return render(<Settings />);
}

describe("connected clusters", () => {
  it("does not call a cluster connected when its credentials were rejected", async () => {
    const { container } = show({
      ...base,
      clusters: [
        {
          secret: "demo/reachable-cluster",
          gates: [],
          reachable: false,
          detail: "the cluster rejected the credentials in this Secret.",
        },
      ],
    });

    await waitFor(() => expect(screen.getByText(/rejected the credentials/)).toBeDefined());
    // These two lines used to sit in the same paragraph: a red line saying the
    // credentials were rejected, and directly beneath it "connected, not yet
    // used". Of the two, the cheerful one is what people believe.
    expect(container.textContent).not.toContain("Connected, not yet used");
    expect(screen.getByText(/Not usable until the credentials work/)).toBeDefined();
  });

  it("tells you how to use a cluster that does work", async () => {
    const { container } = show({
      ...base,
      clusters: [{ secret: "demo/prod-eu", gates: [], reachable: true }],
    });

    await waitFor(() => expect(screen.getByText(/Connected, not yet used/)).toBeDefined());
    expect(container.textContent).toContain("clusterRef: {name: prod-eu}");
    // The snippet and the sentence after it are separate elements, and the
    // space between them belongs to neither — it went missing once already.
    expect(container.textContent).toContain("} to a Gate");
  });

  it("shows the cluster Hecate is in, so the panel is never empty on a working install", async () => {
    show(base);

    // Asked three times whether a cluster was connected, on an installation
    // running inside one and promoting into it. A panel that could only ever
    // list the *extra* clusters answered "none" to the question being asked.
    await waitFor(() => expect(screen.getByText("https://10.0.0.1:443")).toBeDefined());
    expect(screen.getByText("this cluster")).toBeDefined();
    expect(screen.getByText(/no credentials to configure/)).toBeDefined();
  });

  it("says so when Hecate is not in a cluster at all", async () => {
    show({ ...base, home: { inCluster: false } });

    // A developer running the API against their own kubeconfig. The panel has
    // nothing true to show, and inventing an address would be worse.
    await waitFor(() => expect(screen.getByText(/not running inside a cluster/)).toBeDefined());
  });

  it("says what the section means, so an empty one is not read as a failure", async () => {
    const { container } = show(base);

    await waitFor(() => expect(screen.getByText("Connected clusters")).toBeDefined());
    expect(container.textContent).toContain("The cluster Hecate runs in is not one of these");
  });
});
