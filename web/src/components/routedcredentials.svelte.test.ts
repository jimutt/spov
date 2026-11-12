import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The routed credentials section mounted for its wiring: the secret
 * field is empty once a write settles, either way, and the owner choice
 * is an admin's alone.
 *
 * @decision(DL-118)
 */

const stub = vi.hoisted(() => {
  const state = {
    puts: [] as { url: string; body: unknown }[],
    // What the next PUT answers.
    refuse: false,
    // A local server has no organisation.
    orgStatus: 200,
    own: [] as unknown[],
  };
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": status < 400 ? "application/json" : "application/problem+json" },
    });
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const org = req.url.includes("/api/v1/org/");
    if (req.method === "PUT") {
      const body = JSON.parse(await req.text());
      state.puts.push({ url: new URL(req.url).pathname, body });
      if (state.refuse) return json({ title: "Unprocessable Entity", status: 422, detail: "host is a forge's" }, 422);
      if (!org) state.own = [{ route: { ...body.route, scheme: "bearer" } }];
      return json(org ? { routed_credentials: [] } : { route: body.route });
    }
    if (org) return state.orgStatus === 200 ? json({ routed_credentials: [] }) : json({ title: "Not Implemented", status: 501 }, 501);
    return json({ routed_credentials: state.own });
  }) as typeof globalThis.fetch;
  return state;
});

import RoutedCredentials from "./RoutedCredentials.svelte";

async function settle() {
  for (let i = 0; i < 40; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  flushSync();
}

function type(el: Element | null, v: string) {
  const field = el as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;
  field.value = v;
  field.dispatchEvent(new Event(field instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  flushSync();
}

async function opened(admin: boolean) {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(RoutedCredentials, { target, props: { admin } });
  await settle();
  (target.querySelector('[data-testid="routed-add"]') as HTMLButtonElement).click();
  await settle();
  return {
    target,
    close: () => {
      unmount(app);
      target.remove();
    },
  };
}

function fill(target: HTMLElement) {
  type(target.querySelector("#routed-name"), "acme-npm");
  type(target.querySelector("#routed-hosts"), "npm.acme.com");
  type(target.querySelector("#routed-scheme"), "bearer");
  type(target.querySelector("#routed-secret"), "s3cret");
}

function submit(target: HTMLElement) {
  (target.querySelector('[data-testid="routed-form"]') as HTMLFormElement).dispatchEvent(
    new Event("submit", { cancelable: true }),
  );
}

describe("the routed credentials section", () => {
  beforeEach(() => {
    stub.puts.length = 0;
    stub.refuse = false;
    stub.orgStatus = 200;
    stub.own = [];
  });

  it("stores a credential, says it is unchecked and keeps no secret", async () => {
    const { target, close } = await opened(false);
    const secret = target.querySelector("#routed-secret") as HTMLInputElement;
    expect(secret.type).toBe("password");
    expect(secret.getAttribute("autocomplete")).toBe("off");
    fill(target);
    submit(target);
    await settle();
    await settle();
    expect(stub.puts).toEqual([
      {
        url: "/api/v1/me/routed-credentials/acme-npm",
        body: { secret: "s3cret", route: { name: "acme-npm", hosts: ["npm.acme.com"], scheme: "bearer", attach: "always" } },
      },
    ]);
    expect(target.textContent).toContain("Stored acme-npm, unchecked");
    // The form is gone, and a new one opens with nothing typed.
    expect(target.querySelector("#routed-secret")).toBeNull();
    (target.querySelector('[data-testid="routed-add"]') as HTMLButtonElement).click();
    await settle();
    expect((target.querySelector("#routed-secret") as HTMLInputElement).value).toBe("");
    expect(target.innerHTML).not.toContain("s3cret");
    close();
  });

  it("empties the secret after a refused write, keeps the route and shows the server's words", async () => {
    stub.refuse = true;
    const { target, close } = await opened(false);
    fill(target);
    submit(target);
    await settle();
    await settle();
    expect(stub.puts).toHaveLength(1);
    expect(target.textContent).toContain("host is a forge's");
    expect((target.querySelector("#routed-secret") as HTMLInputElement).value).toBe("");
    expect((target.querySelector("#routed-hosts") as HTMLTextAreaElement).value).toBe("npm.acme.com");
    expect((target.querySelector("#routed-name") as HTMLInputElement).value).toBe("acme-npm");
    close();
  });

  it("does not store while a local check fails", async () => {
    const { target, close } = await opened(false);
    fill(target);
    type(target.querySelector("#routed-secret"), "");
    submit(target);
    await settle();
    expect(stub.puts).toEqual([]);
    expect(target.querySelector('[data-testid="routed-problems"]')?.textContent).toContain("Type the secret");
    close();
  });

  it("draws no owner choice for a non-admin", async () => {
    const { target, close } = await opened(false);
    expect(target.querySelector('[data-testid="routed-owner"]')).toBeNull();
    close();
  });

  it("draws no owner choice where the organisation's endpoints answer 501", async () => {
    stub.orgStatus = 501;
    const { target, close } = await opened(true);
    expect(target.querySelector('[data-testid="routed-owner"]')).toBeNull();
    close();
  });

  it("lets an admin store the organisation's", async () => {
    const { target, close } = await opened(true);
    const owner = target.querySelector('[data-testid="routed-owner"]') as HTMLElement;
    expect(owner).not.toBeNull();
    const org = owner.querySelector('input[value="org"]') as HTMLInputElement;
    org.click();
    flushSync();
    fill(target);
    submit(target);
    await settle();
    await settle();
    expect(stub.puts.map((p) => p.url)).toEqual(["/api/v1/org/routed-credentials/acme-npm"]);
    expect(target.textContent).toContain("Stored acme-npm for the organisation, unchecked");
    close();
  });
});
