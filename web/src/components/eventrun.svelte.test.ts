import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it } from "vitest";
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
