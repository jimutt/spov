import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The Settings page's OpenCode sign-in, mounted, for the polling and its
 * end: what is shown while waiting, what changes when the attempt ends,
 * and that leaving the page stops asking.
 *
 * @decision(DL-015)
 */

/**
 * Installed by vi.hoisted: openapi-fetch binds `globalThis.fetch` when
 * `client.ts` is imported. `server` is the fake server's state, which
 * each test sets.
 */
const stub = vi.hoisted(() => {
  const server = {
    connection: { enabled: true, connected: false } as Record<string, unknown>,
    // What the next check of the attempt answers.
    next: { state: "pending" } as Record<string, unknown>,
    checks: 0,
    deletes: 0,
  };
  const attempt = {
    id: "att_1",
    state: "pending",
    verification_url: "https://opencode.ai/console/device?user_code=WXYZ-1234",
    user_code: "WXYZ-1234",
    expires_at: "2026-10-03T12:10:00Z",
    interval: 1,
  };
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
  const fetched = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? (input instanceof Request ? input.method : "GET");
    if (url.includes("/api/v1/me/opencode/connect/")) {
      server.checks++;
      if (server.next.state === "connected") {
        server.connection = { enabled: true, connected: true, org_id: "org_1" };
      }
      return json({ ...attempt, ...server.next });
    }
    if (url.includes("/api/v1/me/opencode/connect") && method === "POST") {
      return json(attempt);
    }
    if (url.includes("/api/v1/me/opencode") && method === "DELETE") {
      server.deletes++;
      server.connection = { enabled: true, connected: false };
      return new Response(null, { status: 204 });
    }
    if (url.includes("/api/v1/me/opencode")) return json(server.connection);
    return new Response(null, { status: 404 });
  });
  globalThis.fetch = fetched as unknown as typeof globalThis.fetch;
  return { server, fetched };
});

import OpenCodeConnect from "./OpenCodeConnect.svelte";

async function settle() {
  for (let i = 0; i < 30; i++) await Promise.resolve();
  flushSync();
}

/** Lets one poll interval pass, then the check's answer land. */
async function tick() {
  await vi.advanceTimersByTimeAsync(1000);
  await settle();
}

function render(onChanged = vi.fn()) {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(OpenCodeConnect, { target, props: { onChanged } });
  return {
    target,
    onChanged,
    text: () => target.textContent?.replace(/\s+/g, " ") ?? "",
    button: (name: string) =>
      [...target.querySelectorAll("button")].find((b) => b.textContent?.trim() === name),
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

beforeEach(() => {
  vi.useFakeTimers();
  stub.server.connection = { enabled: true, connected: false };
  stub.server.next = { state: "pending" };
  stub.server.checks = 0;
  stub.server.deletes = 0;
});

afterEach(() => {
  vi.useRealTimers();
});

describe("connecting OpenCode from Settings", () => {
  it("shows the code, waits, and says when it is connected", async () => {
    const page = render();
    await settle();
    expect(page.text()).toContain("Not connected.");

    page.button("Connect OpenCode")!.click();
    await settle();
    const code = page.target.querySelector("[data-testid=opencode-code]");
    expect(code?.textContent).toBe("WXYZ-1234");
    const link = page.target.querySelector("a") as HTMLAnchorElement;
    expect(link.href).toBe("https://opencode.ai/console/device?user_code=WXYZ-1234");
    expect(link.rel).toContain("noopener");
    expect(page.button("Waiting…")?.disabled).toBe(true);

    await tick();
    expect(stub.server.checks).toBe(1);
    expect(page.text()).toContain("WXYZ-1234");
    expect(page.onChanged).not.toHaveBeenCalled();

    stub.server.next = { state: "connected" };
    await tick();
    expect(page.target.querySelector("[data-testid=opencode-code]")).toBeNull();
    expect(page.text()).toContain("Connected to OpenCode (organization org_1).");
    expect(page.text()).toContain("Sessions can now use the opencode-go/ and opencode/ models.");
    expect(page.onChanged).toHaveBeenCalledOnce();
    // Connected: nothing to connect, something to disconnect.
    expect(page.button("Connect OpenCode")).toBeUndefined();
    expect(page.button("Disconnect")).toBeDefined();

    await tick();
    expect(stub.server.checks).toBe(2);
    page.done();
  });

  it("says why an attempt did not finish, and offers to start again", async () => {
    const page = render();
    await settle();
    page.button("Connect OpenCode")!.click();
    await settle();
    stub.server.next = { state: "failed", reason: "access_denied" };
    await tick();
    expect(page.text()).toContain("The sign-in did not finish: access_denied.");
    expect(page.target.querySelector("[data-testid=opencode-code]")).toBeNull();
    expect(page.button("Connect OpenCode")?.disabled).toBe(false);
    expect(page.onChanged).not.toHaveBeenCalled();
    page.done();
  });

  it("stops asking when the page is left", async () => {
    const page = render();
    await settle();
    page.button("Connect OpenCode")!.click();
    await settle();
    await tick();
    expect(stub.server.checks).toBe(1);

    page.done();
    await tick();
    await tick();
    expect(stub.server.checks).toBe(1);
  });
});

describe("a stored sign-in", () => {
  it("asks for a reconnect when OpenCode refused it, and can be forgotten", async () => {
    stub.server.connection = { enabled: true, connected: false, needs_reconnect: true };
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const page = render();
    await settle();
    expect(page.text()).toContain("OpenCode refused the stored sign-in");
    expect(page.button("Connect OpenCode")).toBeDefined();

    page.button("Disconnect")!.click();
    await settle();
    expect(confirm).toHaveBeenCalledOnce();
    expect(stub.server.deletes).toBe(1);
    expect(page.text()).toContain("Not connected.");
    expect(page.button("Disconnect")).toBeUndefined();
    expect(page.onChanged).toHaveBeenCalledOnce();
    confirm.mockRestore();
    page.done();
  });

  it("is left alone when the confirmation is declined", async () => {
    stub.server.connection = { enabled: true, connected: true };
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const page = render();
    await settle();
    page.button("Disconnect")!.click();
    await settle();
    expect(stub.server.deletes).toBe(0);
    expect(page.text()).toContain("Connected to OpenCode.");
    confirm.mockRestore();
    page.done();
  });

  it("offers no button on a server that cannot hold one", async () => {
    stub.server.connection = { enabled: false, connected: false };
    const page = render();
    await settle();
    expect(page.text()).toContain("This server cannot hold an OpenCode sign-in");
    expect(page.target.querySelectorAll("button")).toHaveLength(0);
    page.done();
  });
});
