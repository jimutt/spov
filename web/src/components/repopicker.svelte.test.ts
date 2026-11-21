import { describe, expect, it } from "vitest";
import { flushSync, mount, unmount } from "svelte";

import RepoPicker from "./RepoPicker.svelte";

/**
 * A public repository a fine-grained token may only read is listed and
 * selectable like any other, with no note under it.
 *
 * @decision(DL-179)
 */
describe("the repository picker", () => {
  it("draws no note on a public repository the token may only read", () => {
    const target = document.createElement("div");
    const app = mount(RepoPicker, {
      target,
      props: {
        repos: [{ platform: "gh", owner: "o", name: "public", private: false, archived: false, via: ["user"], maybe_read_only: true }],
        loaded: true,
        platforms: [{ platform: "gh", tier: "user" }],
      },
    });
    flushSync();
    const row = target.querySelector("label");
    expect(row?.textContent).toContain("o/public");
    expect(row?.textContent).not.toMatch(/Public|push/);
    expect(row?.querySelector("input")?.disabled).toBe(false);
    unmount(app);
  });
});
