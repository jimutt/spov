import { describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The usage page, mounted, for the two things the markup has to keep
 * apart: the sandbox figure and the model-token gap.
 *
 * @decision(DL-116)
 */

vi.hoisted(() => {
  const json = (body: unknown) =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    if (url.pathname === "/api/v1/me/usage") {
      return json({
        since: "2026-11-01T00:00:00Z",
        sessions: 2,
        running_hours: 3.5,
        estimated_cost: 1.25,
        estimated: true,
      });
    }
    return new Response(null, { status: 404 });
  }) as unknown as typeof globalThis.fetch;
});

import Usage from "./Usage.svelte";

async function settle() {
  for (let i = 0; i < 60; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  for (let i = 0; i < 20; i++) await Promise.resolve();
  flushSync();
}

describe("the usage page", () => {
  it("names the figure sandbox cost and marks model token cost as coming soon", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(Usage, { target });
    await settle();

    const said = target.textContent ?? "";
    expect(said).toContain("Sandbox cost");
    expect(said).toContain("at least $1.25");
    expect(said).toContain("How sandbox cost is estimated");
    expect(said).not.toContain("Spend");

    const panel = target.querySelector('[data-testid="token-cost-soon"]');
    expect(panel, "the model token cost gap is not shown").not.toBeNull();
    expect(panel!.textContent).toContain("Model token cost");
    expect(panel!.textContent).toContain("Coming soon");
    expect(panel!.textContent).toContain("Not tracked yet");
    const note = (panel!.textContent ?? "").replace(/\s+/g, " ");
    expect(note).toContain("provider invoice can be much higher");

    unmount(app);
    target.remove();
  });
});
