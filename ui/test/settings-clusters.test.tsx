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

  it("explains an empty list as normal rather than as nothing being connected", async () => {
    show(base);

    // The old wording — "No clusters connected, and no Gate watches a remote
    // one" — is true and reads as a fault, on an installation that is working
    // perfectly. Hecate promotes into its own cluster with its own service
    // account; a connected cluster is only for promoting into a different one.
    await waitFor(() => expect(screen.getByText(/Nothing to connect/)).toBeDefined());
    expect(screen.getByText(/promotes into the cluster it runs in/)).toBeDefined();
  });

  it("says what the section means, so an empty one is not read as a failure", async () => {
    const { container } = show(base);

    await waitFor(() => expect(screen.getByText("Connected clusters")).toBeDefined());
    expect(container.textContent).toContain("The cluster Hecate runs in is not one of these");
  });
});
