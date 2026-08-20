import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/**
 * jsdom has no ResizeObserver, and the chart's ResponsiveContainer needs one.
 *
 * A stub rather than a polyfill: nothing here measures anything, and a chart
 * asked to size itself against a zero-height jsdom element would draw nothing
 * whatever the observer said. What the tests care about is that the page
 * around the chart renders — without this, the chart throws on mount and takes
 * the whole Overview with it.
 */
class NoResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", NoResizeObserver);
