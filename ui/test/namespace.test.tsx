import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import { Shell } from "@/components/shell";
import { api } from "@/lib/api";

// Shell marks the active tab from usePathname, which has no router to read
// outside Next and returns null. Nothing to do with the picker; without it the
// component throws before the control renders.
vi.mock("next/navigation", () => ({ usePathname: () => "/" }));

/**
 * The namespace control used to be a text box, which meant finding the
 * namespace your Gates were in required already knowing it — and the landing
 * namespace is `default`, which almost never holds any.
 *
 * These cover the two ways a picker built from a server list goes wrong, both
 * of which show the same symptom to a user (a control that names nothing) from
 * opposite causes.
 */

function at(query: string) {
  window.history.replaceState({}, "", query);
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("the namespace picker", () => {
  it("offers the namespaces the API says this user can see", async () => {
    at("/?namespace=demo");
    vi.spyOn(api, "namespaces").mockResolvedValue({
      namespaces: ["demo", "staging", "production"],
    });

    render(<Shell>{null}</Shell>);

    const select = (await screen.findByLabelText("Namespace")) as HTMLSelectElement;
    await waitFor(() =>
      expect([...select.options].map((o) => o.value)).toEqual([
        "demo",
        "production",
        "staging",
      ]),
    );
    expect(select.value).toBe("demo");
  });

  it("keeps the current namespace selectable when discovery omits it", async () => {
    // A user following a shared link into a namespace the picker did not list —
    // it holds Gates they can read but the discovery call raced, or a Beacon
    // was deleted between the two. A <select> whose value is absent from its
    // options renders blank, so the page would describe one namespace while the
    // control named none.
    at("/?namespace=somewhere-else");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo"] });

    render(<Shell>{null}</Shell>);

    const select = (await screen.findByLabelText("Namespace")) as HTMLSelectElement;
    await waitFor(() => expect(select.options.length).toBe(2));
    expect(select.value).toBe("somewhere-else");
  });

  it("still works when discovery fails", async () => {
    // Degrading to "just the namespace you are in" is the old text-box
    // behaviour, which is usable. An error banner over a page that otherwise
    // renders is not.
    at("/?namespace=demo");
    vi.spyOn(api, "namespaces").mockRejectedValue(new Error("boom"));

    render(<Shell>{null}</Shell>);

    const select = (await screen.findByLabelText("Namespace")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("demo"));
    expect(select.options.length).toBe(1);
  });
});
