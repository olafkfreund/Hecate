import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import { Shell } from "@/components/shell";
import Gates from "@/app/gates/page";
import { api } from "@/lib/api";
import { setQueryParam } from "@/lib/browser";

// Shell reads usePathname, which has no router outside Next and returns null.
//
// Reading window.location rather than returning a constant: the Shell now uses
// the path for more than marking the active tab — the namespace picker is
// hidden on the cluster-wide Overview at "/" — so a mock pinned to one path
// would decide the outcome of every test using it, whatever URL that test set.
vi.mock("next/navigation", () => ({ usePathname: () => window.location.pathname }));

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
    at("/gates/?namespace=demo");
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
    at("/gates/?namespace=somewhere-else");
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
    at("/gates/?namespace=demo");
    vi.spyOn(api, "namespaces").mockRejectedValue(new Error("boom"));

    render(<Shell>{null}</Shell>);

    const select = (await screen.findByLabelText("Namespace")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("demo"));
    expect(select.options.length).toBe(1);
  });
});

describe("where the picker belongs", () => {
  it("is absent on the Overview, which spans every namespace", async () => {
    at("/");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo"] });

    render(<Shell>{null}</Shell>);

    // A control that changes nothing on the page it is shown on reads as
    // broken rather than as not applicable.
    expect(screen.queryByLabelText("Namespace")).toBeNull();
  });

  it("is present on a page that is about one namespace", async () => {
    at("/gates/?namespace=demo");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo"] });

    render(<Shell>{null}</Shell>);

    expect(await screen.findByLabelText("Namespace")).toBeDefined();
  });
});

describe("where an addressless visit lands", () => {
  it("moves to a namespace that has something in it", async () => {
    at("/gates/");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo", "team-b"] });

    render(<Shell>{null}</Shell>);

    // `default` was only ever a placeholder, and is almost never where anyone's
    // Gates are — so the first thing every page said to a new arrival was that
    // there was nothing to see.
    await waitFor(() =>
      expect(new URLSearchParams(window.location.search).get("namespace")).toBe("demo"),
    );

  });

  it("makes the page underneath load the namespace it landed on", async () => {
    at("/gates/");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo"] });
    const gates = vi.spyOn(api, "gates").mockResolvedValue([]);

    render(
      <Shell>
        <Gates />
      </Shell>,
    );

    // The point of the whole exercise: the page loads the namespace it landed
    // on, not the one it started at.
    await waitFor(() => expect(gates).toHaveBeenCalledWith("demo"));
  });

  it("never overrides a namespace someone was sent to", async () => {
    at("/gates/?namespace=team-b");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: ["demo", "team-b"] });

    render(<Shell>{null}</Shell>);

    // A link to a Gate has to open on that Gate for whoever it was sent to, not
    // on whichever namespace their server happened to list first.
    await waitFor(() => expect(screen.getByLabelText("Namespace")).toBeDefined());
    expect(new URLSearchParams(window.location.search).get("namespace")).toBe("team-b");
  });

  it("leaves the page working when there is nowhere better to go", async () => {
    at("/gates/");
    vi.spyOn(api, "namespaces").mockResolvedValue({ namespaces: [] });

    render(<Shell>{null}</Shell>);

    await waitFor(() => expect(screen.getByLabelText("Namespace")).toBeDefined());
    // No namespace discovered is the state this replaced, not a fault.
    expect(new URLSearchParams(window.location.search).has("namespace")).toBe(false);
  });

  it("stays put when discovery fails outright", async () => {
    at("/gates/");
    vi.spyOn(api, "namespaces").mockRejectedValue(new Error("nope"));

    render(<Shell>{null}</Shell>);

    await waitFor(() => expect(screen.getByLabelText("Namespace")).toBeDefined());
    expect(new URLSearchParams(window.location.search).has("namespace")).toBe(false);
  });
});

describe("setQueryParam", () => {
  it("tells the page, because replaceState does not", () => {
    at("/gates/?namespace=demo");
    const heard = vi.fn();
    window.addEventListener("popstate", heard);

    setQueryParam("namespace", "team-b");

    expect(new URLSearchParams(window.location.search).get("namespace")).toBe("team-b");
    // history.replaceState deliberately fires no popstate, and useQueryParam
    // subscribes to popstate. Without an explicit one this function would
    // change the address bar and leave every reader of it showing the old
    // value.
    //
    // Tested here rather than through a page, because through a page it cannot
    // fail: useSyncExternalStore re-reads its snapshot on every commit, so any
    // unrelated state update anywhere in the tree picks the new value up. That
    // makes the page look right for a reason that has nothing to do with this
    // function, and would keep looking right until the day nothing else
    // happened to commit.
    expect(heard).toHaveBeenCalled();
    window.removeEventListener("popstate", heard);
  });

  it("does nothing when the value is already what was asked for", () => {
    at("/gates/?namespace=demo");
    const heard = vi.fn();
    window.addEventListener("popstate", heard);

    setQueryParam("namespace", "demo");

    // Otherwise landing would notify on every render that reached it, and each
    // notification costs every page on screen a refetch.
    expect(heard).not.toHaveBeenCalled();
    window.removeEventListener("popstate", heard);
  });
});
