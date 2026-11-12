import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The session form mounted for the template picker's wiring: None sends
 * no template, a chosen one is sent, the picker is a link when there
 * are none and nothing where the server keeps none.
 *
 * @decision(DL-121)
 */

const stub = vi.hoisted(() => {
  const state = {
    posts: [] as unknown[],
    templates: [] as unknown[],
    templatesStatus: 200,
  };
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const path = new URL(req.url).pathname;
    if (req.method === "POST") {
      state.posts.push(JSON.parse(await req.text()));
      return json({ id: "01ABC" }, 202);
    }
    if (path === "/api/v1/me/templates") {
      return state.templatesStatus === 200 ? json({ templates: state.templates }) : json({ status: 501 }, 501);
    }
    if (path === "/api/v1/repos") {
      return json({ repos: [{ platform: "github", owner: "acme", name: "app" }], platforms: [] });
    }
    if (path === "/api/v1/models") return json({ providers: [] });
    return json({ user_id: "u", org_id: "o", admin: false });
  }) as typeof globalThis.fetch;
  return state;
});

import NewSession from "./NewSession.svelte";
import { nav } from "../lib/api/client";

async function settle() {
  for (let i = 0; i < 40; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  flushSync();
}

async function mounted() {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(NewSession, { target });
  await settle();
  return {
    target,
    close: () => {
      unmount(app);
      target.remove();
    },
  };
}

async function start(target: HTMLElement) {
  const repo = target.querySelector('input[type="checkbox"]') as HTMLInputElement;
  repo.click();
  flushSync();
  (target.querySelector("form") as HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
  await settle();
}

const acme = { name: "acme", egress: "install", credentials: ["npm", "maven"] };

describe("the template picker on a launch form", () => {
  const went: string[] = [];
  beforeEach(() => {
    stub.posts.length = 0;
    stub.templates = [acme];
    stub.templatesStatus = 200;
    went.length = 0;
    vi.spyOn(nav, "go").mockImplementation((url) => void went.push(url));
    localStorage.clear();
  });

  it("opens on None and sends no template", async () => {
    const { target, close } = await mounted();
    const select = target.querySelector("#session-template") as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(select.options[0].textContent).toBe("None");
    await start(target);
    expect(stub.posts).toEqual([{ repos: [{ platform: "github", owner: "acme", name: "app" }] }]);
    expect(went).toEqual(["/session/?id=01ABC"]);
    close();
  });

  it("sends a chosen template, shows its summary and remembers nothing", async () => {
    const { target, close } = await mounted();
    const select = target.querySelector("#session-template") as HTMLSelectElement;
    select.value = "acme";
    select.dispatchEvent(new Event("change", { bubbles: true }));
    flushSync();
    expect(target.querySelector('[data-testid="template-summary"]')?.textContent).toContain("Egress install, 2 credentials.");
    await start(target);
    expect(stub.posts).toEqual([{ repos: [{ platform: "github", owner: "acme", name: "app" }], template: "acme" }]);
    expect(localStorage.length).toBe(0);
    close();
    // The next load is None again.
    const again = await mounted();
    expect((again.target.querySelector("#session-template") as HTMLSelectElement).value).toBe("");
    again.close();
  });

  it("is one line linking to Settings when there are no templates", async () => {
    stub.templates = [];
    const { target, close } = await mounted();
    expect(target.querySelector("#session-template")).toBeNull();
    const link = target.querySelector('[data-testid="template-picker"] a') as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/settings/");
    close();
  });

  it("draws nothing where the server keeps no templates", async () => {
    stub.templatesStatus = 501;
    const { target, close } = await mounted();
    expect(target.querySelector('[data-testid="template-picker"]')).toBeNull();
    await start(target);
    expect(stub.posts).toEqual([{ repos: [{ platform: "github", owner: "acme", name: "app" }] }]);
    close();
  });
});
