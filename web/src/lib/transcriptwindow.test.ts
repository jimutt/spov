import { describe, expect, it } from "vitest";
import { WindowSize, earlier, earlierLabel, held, windowOf } from "./transcriptwindow";

const blocks = (n: number, from = 0) => Array.from({ length: n }, (_, i) => ({ key: `b${from + i}` }));

describe("windowOf", () => {
  it("draws everything while the transcript fits", () => {
    const all = blocks(WindowSize);
    expect(windowOf(all, undefined)).toEqual({ shown: all, hidden: 0 });
  });

  it("slides to the newest blocks while it follows the tail", () => {
    const w = windowOf(blocks(WindowSize + 50), undefined);
    expect(w.hidden).toBe(50);
    expect(w.shown).toHaveLength(WindowSize);
    expect(w.shown[0].key).toBe("b50");
  });

  it("keeps a held start as the transcript grows below it", () => {
    const start = held(blocks(250), undefined);
    const grown = windowOf(blocks(400), start);
    expect(grown.hidden).toBe(50);
    expect(grown.shown).toHaveLength(350);
  });

  it("falls back to the index taken when the start's block is gone", () => {
    const w = windowOf([...blocks(10), ...blocks(300, 100)], { key: "missing", index: 40 });
    expect(w.hidden).toBe(40);
  });

  it("never starts past the last block", () => {
    expect(windowOf(blocks(5), { key: "missing", index: 99 }).shown).toHaveLength(1);
    expect(windowOf([], { key: "b0", index: 0 })).toEqual({ shown: [], hidden: 0 });
  });
});

describe("held", () => {
  it("holds a transcript that still fits, so its growth slides nothing away", () => {
    const start = held(blocks(150), undefined);
    expect(start).toEqual({ key: "b0", index: 0 });
    const grown = windowOf(blocks(260), start);
    expect(grown.hidden).toBe(0);
    expect(grown.shown[0].key).toBe("b0");
  });

  it("is nothing to hold before there are blocks", () => {
    expect(held([], undefined)).toBeUndefined();
  });

  it("holds the window where it starts", () => {
    expect(held(blocks(260), undefined)).toEqual({ key: "b60", index: 60 });
  });

  it("keeps a start already held", () => {
    const start = { key: "b5", index: 5 };
    expect(held(blocks(900), start)).toBe(start);
  });
});

describe("earlier", () => {
  it("moves the start back one window, and stops at the first block", () => {
    const all = blocks(1000);
    const once = earlier(all, undefined);
    expect(once).toEqual({ key: "b600", index: 600 });
    expect(earlier(all, once)).toEqual({ key: "b400", index: 400 });
    expect(earlier(all, { key: "b150", index: 150 })).toEqual({ key: "b0", index: 0 });
    expect(windowOf(all, earlier(all, { key: "b150", index: 150 })).hidden).toBe(0);
  });
});

describe("earlierLabel", () => {
  it("says how many are not drawn", () => {
    expect(earlierLabel(1)).toBe("Show earlier (1 more)");
    expect(earlierLabel(4870)).toBe("Show earlier (4870 more)");
  });
});
