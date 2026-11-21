import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import EventRun from "./EventRun.svelte";
import type { EventEntry } from "../lib/timeline";

function event(key: string, name: string, extra: Partial<EventEntry> = {}): EventEntry {
  return { kind: "event", key, at: 0, seq: 0, name, text: "", seam: false, ...extra };
}

const thought = "I should read the player first.\n\nThe seek bar rounds on every frame, which is where the drift comes from, so the fix belongs there rather than in the timer.";
const run: EventEntry[] = [
  event("t1", "session.reasoning.ended", { text: thought }),
  event("c1", "session.tool.input.started", {
    calls: { name: "read", input: '{"filePath":"src/player/seek.ts","limit":200}', state: "done", error: "" },
  }),
];

function draw(open = true) {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(EventRun, { target, props: { events: run, open, ontoggle: () => {} } });
  flushSync();
  return {
    target,
    call: () => target.querySelector('[data-testid="tool-call"] button') as HTMLButtonElement,
    detail: () => target.querySelector('[data-testid="tool-detail"]'),
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("an opened run of events", () => {
  // @decision(DL-158)
  it("draws a thought whole, its paragraphs kept", () => {
    const r = draw();
    const t = r.target.querySelector('[data-testid="thought"]')!;
    expect(t.textContent?.trim()).toBe(thought);
    expect(t.className).not.toContain("line-clamp");
    r.done();
  });

  // @decision(DL-158)
  it("keeps each tool call to one line until it is opened on its own", () => {
    const r = draw();
    expect(r.call().textContent).toContain("src/player/seek.ts");
    expect(r.call().getAttribute("aria-expanded")).toBe("false");
    expect(r.detail()).toBeNull();

    r.call().click();
    flushSync();
    expect(r.call().getAttribute("aria-expanded")).toBe("true");
    expect(r.detail()?.textContent).toBe('{\n  "filePath": "src/player/seek.ts",\n  "limit": 200\n}');

    r.call().click();
    flushSync();
    expect(r.detail()).toBeNull();
    r.done();
  });

  // @decision(DL-058)
  it("draws nothing of a folded run", () => {
    const r = draw(false);
    expect(r.target.querySelector('[data-testid="thought"]')).toBeNull();
    expect(r.target.querySelector('[data-testid="tool-call"]')).toBeNull();
    r.done();
  });
});

/** narrow stands in for a screen below `lg`, until the test ends. */
function narrow(): () => void {
  const was = window.matchMedia;
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
  return () => {
    window.matchMedia = was;
  };
}

/** A run whose open state lives in the test, as the page keeps it. */
function drawToggling(props: { live?: boolean } = {}) {
  const target = document.createElement("div");
  document.body.append(target);
  const state = $state({ open: false });
  let toggles = 0;
  const app = mount(EventRun, {
    target,
    props: {
      events: run,
      get open() {
        return state.open;
      },
      ontoggle: () => {
        toggles++;
        state.open = !state.open;
      },
      ...props,
    },
  });
  flushSync();
  return {
    target,
    state,
    toggles: () => toggles,
    head: () => target.querySelector('[data-testid="run-head"]') as HTMLElement,
    summary: () => target.querySelector('[data-testid="run-head"] button') as HTMLButtonElement,
    sheet: () => document.querySelector('[data-testid="run-sheet"]'),
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("a long run on a narrow screen", () => {
  // @decision(DL-167)
  it("opens in a sheet over the page and leaves the transcript as it was", () => {
    const restore = narrow();
    const r = drawToggling();
    r.summary().click();
    flushSync();
    expect(r.sheet()).not.toBeNull();
    expect(r.sheet()!.querySelector('[data-testid="thought"]')?.textContent?.trim()).toBe(thought);
    // Nothing of the run is drawn in the transcript itself.
    const inline = [...r.target.querySelectorAll('[data-testid="thought"]')].filter((t) => !t.closest('[data-testid="run-sheet"]'));
    expect(inline).toHaveLength(0);
    expect(document.body.style.overflow).toBe("hidden");
    r.done();
    restore();
  });

  // @decision(DL-167)
  it("closes on Escape and on Close, taking back the history entry it pushed", async () => {
    const restore = narrow();
    const r = drawToggling();
    const depth = history.length;
    r.summary().click();
    flushSync();
    expect(history.length).toBe(depth + 1);

    window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    await new Promise((done) => window.addEventListener("popstate", done, { once: true }));
    flushSync();
    expect(r.sheet()).toBeNull();
    expect(r.state.open).toBe(false);
    expect(document.body.style.overflow).toBe("");

    r.summary().click();
    flushSync();
    (r.sheet()!.querySelector("button") as HTMLButtonElement).click();
    await new Promise((done) => window.addEventListener("popstate", done, { once: true }));
    flushSync();
    expect(r.sheet()).toBeNull();
    expect(r.toggles()).toBe(4);
    r.done();
    restore();
  });

  // @decision(DL-167)
  it("closes on Back", async () => {
    const restore = narrow();
    const r = drawToggling();
    r.summary().click();
    flushSync();
    history.back();
    await new Promise((done) => window.addEventListener("popstate", done, { once: true }));
    flushSync();
    expect(r.sheet()).toBeNull();
    expect(r.state.open).toBe(false);
    r.done();
    restore();
  });
});

describe("a long run whose screen changes width", () => {
  // @decision(DL-167)
  it("stays a sheet once opened as one, and an inline run never becomes one", () => {
    let matches = false;
    const listeners: (() => void)[] = [];
    const was = window.matchMedia;
    window.matchMedia = ((query: string) => ({
      get matches() {
        return matches;
      },
      media: query,
      addEventListener: (_: string, f: () => void) => listeners.push(f),
      removeEventListener: () => {},
    })) as unknown as typeof window.matchMedia;
    const depth = history.length;
    matches = true;
    const r = drawToggling();
    r.summary().click();
    flushSync();
    expect(r.sheet()).toBeNull();
    matches = false;
    listeners.forEach((f) => f());
    flushSync();
    expect(r.sheet()).toBeNull();
    expect(history.length).toBe(depth);
    expect(document.body.style.overflow).toBe("");
    r.done();
    window.matchMedia = was;
  });
});

describe("closing a sheet twice", () => {
  // @decision(DL-167)
  it("goes back once", async () => {
    const restore = narrow();
    const r = drawToggling();
    r.summary().click();
    flushSync();
    const back = vi.spyOn(history, "back");
    (r.sheet()!.querySelector("button") as HTMLButtonElement).click();
    window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    expect(back).toHaveBeenCalledTimes(1);
    await new Promise((done) => window.addEventListener("popstate", done, { once: true }));
    flushSync();
    expect(r.sheet()).toBeNull();
    back.mockRestore();
    r.done();
    restore();
  });
});

describe("a long run on a wide screen", () => {
  // @decision(DL-167)
  it("opens in place under a sticky summary", () => {
    const r = drawToggling();
    expect(r.head().className).not.toContain("sticky");
    r.summary().click();
    flushSync();
    expect(r.sheet()).toBeNull();
    expect(r.target.querySelector('[data-testid="thought"]')).not.toBeNull();
    expect(r.head().className).toContain("sticky");
    r.done();
  });
});

describe("the newest run while the agent works", () => {
  // @decision(DL-168)
  it("names its latest action while folded", () => {
    const r = drawToggling({ live: true });
    expect(r.target.querySelector('[data-testid="run-latest"]')?.textContent).toContain("read src/player/seek.ts");
    r.summary().click();
    flushSync();
    expect(r.target.querySelector('[data-testid="run-latest"]')).toBeNull();
    r.done();
  });

  // @decision(DL-168)
  it("says only its count once it is no longer the live one", () => {
    const r = drawToggling();
    expect(r.target.querySelector('[data-testid="run-latest"]')).toBeNull();
    r.done();
  });
});
