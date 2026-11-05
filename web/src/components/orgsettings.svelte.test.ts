import { describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The Organisation section mounted for its wiring: it draws the App's
 * status, stores one through PUT, and clears the key it sent.
 *
 * @decision(DL-042)
 */

const stub = vi.hoisted(() => {
  const puts: unknown[] = [];
  let stored = false;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    if (req.method === "PUT") {
      puts.push(JSON.parse(await req.text()));
      stored = true;
    }
    const platform = stored
      ? { platform: "gh", source: "stored", app_id: "123", updated_by: "me@x", check: { at: "", repo_count: 2, account: "acme" } }
      : { platform: "gh", source: "none" };
    return new Response(JSON.stringify({ platforms: [platform], events: [] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  }) as typeof globalThis.fetch;
  return { puts };
});

import OrgSettings from "./OrgSettings.svelte";

async function settle() {
  for (let i = 0; i < 40; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  flushSync();
}

function type(el: HTMLInputElement | HTMLTextAreaElement, v: string) {
  el.value = v;
  el.dispatchEvent(new Event("input", { bubbles: true }));
  flushSync();
}

describe("the Organisation section", () => {
  it("stores a checked App and forgets the key", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(OrgSettings, { target });
    await settle();
    expect(target.textContent).toContain("No App is configured");
    type(target.querySelector("#app-id-gh") as HTMLInputElement, "123");
    const key = target.querySelector("#app-key-gh") as HTMLTextAreaElement;
    type(key, "-----BEGIN RSA PRIVATE KEY-----\nabc");
    (target.querySelector('[data-testid="org-github-form"]') as HTMLFormElement).dispatchEvent(
      new Event("submit", { cancelable: true }),
    );
    await settle();
    await settle();
    expect(stub.puts).toEqual([{ app_id: "123", private_key: "-----BEGIN RSA PRIVATE KEY-----\nabc" }]);
    expect(target.querySelector('[data-testid="org-github-check"]')?.textContent).toContain("Installed on acme");
    expect((target.querySelector("#app-key-gh") as HTMLTextAreaElement).value).toBe("");
    unmount(app);
    target.remove();
  });
});
