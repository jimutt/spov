import { describe, expect, it } from "vitest";
import { mount, unmount } from "svelte";
import LimitMeter from "./LimitMeter.svelte";

/**
 * The meter mounted: the count is always the real one, and the ticks stay
 * one bounded row however large a limit the server reports.
 *
 * @decision(DL-060) @decision(DL-162) @decision(DL-164)
 */
function render(props: { used: number; limit: number; noun: string }) {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(LimitMeter, { target, props: { ...props, testid: "limit" } });
  return {
    text: () => target.querySelector('[data-testid="limit"]')?.textContent?.replace(/\s+/g, " ").trim(),
    title: () => target.querySelector('[data-testid="limit"]')?.getAttribute("title"),
    ticks: () => target.querySelectorAll("i").length,
    filled: () => target.querySelectorAll("i.bg-olive, i.bg-gold").length,
    tickClass: () => target.querySelector("i")?.className ?? "",
    rowClass: () => target.querySelector("[aria-hidden='true']")?.className ?? "",
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("the limit meter", () => {
  it("shows the count and one tick per unit up to the limit", () => {
    const page = render({ used: 2, limit: 5, noun: "sessions" });
    expect(page.text()).toBe("2 of 5 sessions");
    expect(page.ticks()).toBe(5);
    expect(page.filled()).toBe(2);
    expect(page.tickClass()).not.toContain("max-sm:w-1.5");
    expect(page.title()).toBeNull();
    page.done();
  });

  it("marks a limit that is in use", () => {
    const page = render({ used: 4, limit: 4, noun: "jobs in progress" });
    expect(page.text()).toBe("4 of 4 jobs in progress");
    expect(page.filled()).toBe(4);
    expect(page.title()).toContain("At your limit");
    page.done();
  });

  it("keeps a very large limit one bounded row, naming the real count", () => {
    const page = render({ used: 120, limit: 500, noun: "sessions" });
    expect(page.text()).toBe("120 of 500 sessions");
    expect(page.ticks()).toBe(24);
    // 120 of 500 is a little under a quarter of the way, and 6 of 24.
    expect(page.filled()).toBe(6);
    // A long row shrinks on a phone instead of squeezing the words.
    expect(page.tickClass()).toContain("max-sm:w-1.5");
    expect(page.rowClass()).toContain("max-sm:gap-px");
    page.done();
  });

  it("shows the count alone without a limit", () => {
    const page = render({ used: 1, limit: 0, noun: "sessions" });
    expect(page.text()).toBe("1 sessions");
    expect(page.ticks()).toBe(0);
    expect(page.filled()).toBe(0);
    page.done();
  });
});
