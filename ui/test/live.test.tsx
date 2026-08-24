import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import Passages from "@/app/passages/page";
import { api, type Passage } from "@/lib/api";

/**
 * A stand-in for the browser's EventSource, which jsdom does not provide.
 *
 * Deliberately not a full implementation: the hook uses four members, and a
 * fake that pretended to do more would be a second implementation of a thing
 * the browser already has.
 */
class FakeEventSource {
  static last: FakeEventSource | null = null;
  static opened: string[] = [];

  listeners = new Map<string, Set<(e: MessageEvent) => void>>();
  closed = false;
  onerror: (() => void) | null = null;

  constructor(readonly url: string) {
    FakeEventSource.last = this;
    FakeEventSource.opened.push(url);
  }

  addEventListener(kind: string, fn: (e: MessageEvent) => void) {
    if (!this.listeners.has(kind)) this.listeners.set(kind, new Set());
    this.listeners.get(kind)!.add(fn);
  }

  removeEventListener(kind: string, fn: (e: MessageEvent) => void) {
    this.listeners.get(kind)?.delete(fn);
  }

  close() {
    this.closed = true;
  }

  /** emit is the server saying something happened. */
  emit(kind: string) {
    for (const fn of this.listeners.get(kind) ?? []) {
      fn(new MessageEvent(kind, { data: '{"namespace":"uidemo"}' }));
    }
  }
}

function withEventSource() {
  FakeEventSource.last = null;
  FakeEventSource.opened = [];
  vi.stubGlobal("EventSource", FakeEventSource);
}

const passage: Passage = {
  metadata: { name: "app-1-production-xk4", namespace: "uidemo" },
  spec: { gate: "production", bundle: "app-1" },
  status: { phase: "Running" },
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("live updates", () => {
  it("reloads the page when the server says the namespace changed", async () => {
    withEventSource();
    window.history.replaceState({}, "", "/passages/?namespace=uidemo");
    const list = vi.spyOn(api, "passages").mockResolvedValue([passage]);

    render(<Passages />);
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));

    // The stream carries no resources — only the fact that something moved.
    // The page refetches through the endpoint it already uses, so the server
    // decides afresh whether this caller may see the answer.
    FakeEventSource.last!.emit("changed");
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  });

  // A list page spans every namespace, so it watches every namespace. Watching
  // one would leave it stale whenever anything moved anywhere else — and the
  // failure would be invisible, because a page that quietly stops updating
  // looks exactly like a page where nothing has happened.
  it("watches every namespace, not one of them", async () => {
    withEventSource();
    window.history.replaceState({}, "", "/passages/");
    vi.spyOn(api, "passages").mockResolvedValue([]);

    render(<Passages />);

    await waitFor(() => expect(FakeEventSource.opened.length).toBe(1));
    expect(FakeEventSource.opened[0]).toBe("/api/v1alpha1/watch");
  });

  // A stale `?namespace=` in someone's bookmark must not narrow what they see:
  // the pages no longer take one, so a leftover parameter is not an instruction.
  it("ignores a namespace left in the URL", async () => {
    withEventSource();
    window.history.replaceState({}, "", "/passages/?namespace=team-b");
    vi.spyOn(api, "passages").mockResolvedValue([]);

    render(<Passages />);

    await waitFor(() => expect(FakeEventSource.opened.length).toBe(1));
    expect(FakeEventSource.opened[0]).not.toContain("team-b");
  });

  it("closes the stream when the page goes away", async () => {
    withEventSource();
    window.history.replaceState({}, "", "/passages/?namespace=uidemo");
    vi.spyOn(api, "passages").mockResolvedValue([passage]);

    const { unmount } = render(<Passages />);
    await waitFor(() => expect(FakeEventSource.last).not.toBeNull());

    unmount();
    // Otherwise every navigation leaves a connection open against the API, and
    // a long session ends up holding one per page it has visited.
    expect(FakeEventSource.last!.closed).toBe(true);
  });

  it("still shows the page when the browser has no EventSource", async () => {
    // No stub at all: jsdom has no EventSource, which is the case a server-side
    // render or an old browser presents. Live updates are an improvement on a
    // working page, never a requirement for one.
    expect(typeof EventSource).toBe("undefined");
    window.history.replaceState({}, "", "/passages/?namespace=uidemo");
    vi.spyOn(api, "passages").mockResolvedValue([passage]);

    render(<Passages />);

    await waitFor(() => expect(screen.getByText("app-1")).toBeDefined());
  });
});
