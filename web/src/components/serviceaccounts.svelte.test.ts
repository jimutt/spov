import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The Service accounts section mounted for its wiring: it draws the list
 * as text, creates an account and shows a refusal beside the field,
 * issues a token that is shown once and gone after Done, revokes a
 * token, and offers Delete again after a delete that answered 500. The
 * behaviour is in ../lib/serviceaccounts.test.ts.
 *
 * @decision(DL-142)
 */

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

const SECRET = "spov_ThisIsTheIssuedTokenText0123456789";

const stub = vi.hoisted(() => {
  const state = {
    sent: [] as Sent[],
    accounts: [] as unknown[],
    tokens: [] as unknown[],
    // The answer to the next write, else a default success.
    next: [] as { status: number; body?: unknown }[],
  };
  const json = (status: number, body: unknown) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": status < 300 ? "application/json" : "application/problem+json" },
    });
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    const text = req.method === "POST" || req.method === "PATCH" ? await req.text() : "";
    state.sent.push({ method: req.method, path: url.pathname, body: text ? JSON.parse(text) : undefined });
    if (req.method === "GET") {
      if (url.pathname.endsWith("/tokens")) return json(200, { tokens: state.tokens });
      return json(200, { accounts: state.accounts });
    }
    const answer = state.next.shift();
    if (answer) return json(answer.status, answer.body);
    if (req.method === "DELETE") return new Response(null, { status: 204 });
    if (url.pathname.endsWith("/tokens")) {
      return json(201, {
        id: "01JCZ8Y3N5Q2W9V7T6R4P3M2K9", name: "deploy", created_at: "2026-11-17T00:00:00Z",
        expires_at: "2027-11-17T00:00:00Z", issued_by: { id: "u1", label: "@root" }, token: "spov_ThisIsTheIssuedTokenText0123456789",
      });
    }
    return json(201, {
      id: "01JCZ8Y3N5Q2W9V7T6R4P3M2KA", name: "new-bot", git_name: "new-bot", git_email: "new-bot@service.invalid",
      created_at: "2026-11-17T00:00:00Z", tokens: 0,
    });
  }) as typeof globalThis.fetch;
  return state;
});

import ServiceAccounts from "./ServiceAccounts.svelte";

async function settle() {
  for (let i = 0; i < 40; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  flushSync();
}

function type(el: HTMLInputElement, v: string) {
  el.value = v;
  el.dispatchEvent(new Event("input", { bubbles: true }));
  flushSync();
}

const release = {
  id: "01JC0000000000000000000001", name: "release-bot", git_name: "<b>Release</b>", git_email: "rel@example.com",
  created_at: "2026-11-01T00:00:00Z", tokens: 2, last_used_at: "2026-11-10T09:00:00Z",
};
const alpha = {
  id: "01JC0000000000000000000002", name: "alpha", git_name: "alpha", git_email: "alpha@service.invalid",
  created_at: "2026-11-01T00:00:00Z", tokens: 0,
};
const gone = {
  id: "01JC0000000000000000000003", name: "aaa-old", git_name: "aaa-old", git_email: "aaa-old@service.invalid",
  created_at: "2026-11-01T00:00:00Z", tokens: 0, disabled_at: "2026-11-05T00:00:00Z",
};
const ciToken = {
  id: "01JCZ8Y3N5Q2W9V7T6R4P3M2K1", name: "ci", created_at: "2026-11-02T00:00:00Z",
  issued_by: { id: "u1", label: "@root" },
};

async function mounted() {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(ServiceAccounts, { target });
  await settle();
  return {
    target,
    q: (id: string) => target.querySelector(`[data-testid="${id}"]`),
    all: (id: string) => [...target.querySelectorAll(`[data-testid="${id}"]`)],
    row: (name: string) =>
      [...target.querySelectorAll('[data-testid="service-account"]')].find(
        (r) => r.querySelector("span")?.textContent === name,
      ) as HTMLElement,
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

function writes() {
  return stub.sent.filter((s) => s.method !== "GET");
}
function listReads() {
  return stub.sent.filter((s) => s.method === "GET" && s.path === "/api/v1/service-accounts").length;
}
function submit(form: Element | null) {
  (form as HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
}

describe("the Service accounts section", () => {
  beforeEach(() => {
    stub.sent.length = 0;
    stub.next.length = 0;
    stub.accounts = [release, gone, alpha];
    stub.tokens = [ciToken];
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("lists the accounts as text, deleted ones last with only Delete again", async () => {
    const p = await mounted();
    const rows = p.all("service-account");
    expect(rows.map((r) => r.querySelector("span")?.textContent)).toEqual(["alpha", "release-bot", "aaa-old"]);
    const rel = p.row("release-bot");
    expect(rel.textContent).toContain("<b>Release</b> <rel@example.com>");
    expect(rel.querySelector("b")).toBeNull();
    expect(rel.textContent).toContain("2 usable tokens");
    expect(rel.textContent).toContain("Last used");
    expect(p.row("alpha").textContent).toContain("Never used");
    const old = p.row("aaa-old");
    expect(old.querySelector('[data-testid="service-account-deleted"]')?.textContent).toContain("Deleted");
    expect(old.querySelector('[data-testid="service-account-delete-again"]')).not.toBeNull();
    expect(old.querySelector('[data-testid="service-account-tokens-toggle"]')).toBeNull();
    expect(old.querySelector('[data-testid="service-account-delete"]')).toBeNull();
    expect(old.querySelector('[data-testid="service-account-edit-open"]')).toBeNull();
    p.done();
  });

  it("creates an account, leaving empty author fields out, and reads the list again", async () => {
    const p = await mounted();
    const before = listReads();
    type(p.target.querySelector("#service-account-name") as HTMLInputElement, "new-bot");
    submit(p.q("service-account-create"));
    await settle();
    await settle();
    expect(writes()).toEqual([{ method: "POST", path: "/api/v1/service-accounts", body: { name: "new-bot" } }]);
    expect(p.q("service-account-created")?.textContent).toContain("Created new-bot");
    expect((p.target.querySelector("#service-account-name") as HTMLInputElement).value).toBe("");
    expect(listReads()).toBe(before + 1);
    p.done();
  });

  it("does not send a name of the wrong shape, and shows a refusal in the server's words", async () => {
    const p = await mounted();
    const name = p.target.querySelector("#service-account-name") as HTMLInputElement;
    type(name, "../users");
    expect(p.q("service-account-name-note")?.textContent).toContain("is not a service account name");
    submit(p.q("service-account-create"));
    await settle();
    expect(writes()).toEqual([]);

    stub.next.push({
      status: 409,
      body: { title: "Conflict", status: 409, detail: "a service account is already named aaa-old; a deleted one keeps its name" },
    });
    type(name, "aaa-old");
    submit(p.q("service-account-create"));
    await settle();
    await settle();
    expect(p.q("service-account-create-failure")?.textContent).toContain("a deleted one keeps its name");
    expect(name.value).toBe("aaa-old");
    p.done();
  });

  it("changes the author with only the field that changed", async () => {
    const p = await mounted();
    const rel = p.row("release-bot");
    (rel.querySelector('[data-testid="service-account-edit-open"]') as HTMLButtonElement).click();
    flushSync();
    type(p.target.querySelector("#service-account-edit-email") as HTMLInputElement, "ci@example.com");
    submit(p.q("service-account-edit"));
    await settle();
    await settle();
    expect(writes()).toEqual([
      { method: "PATCH", path: "/api/v1/service-accounts/release-bot", body: { git_email: "ci@example.com" } },
    ]);
    expect(p.q("service-account-edit")).toBeNull();
    p.done();
  });

  it("shows an issued token once, nowhere else, and forgets it on Done", async () => {
    const log = vi.spyOn(console, "log");
    const warn = vi.spyOn(console, "warn");
    const error = vi.spyOn(console, "error");
    const p = await mounted();
    const rel = p.row("release-bot");
    (rel.querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
    await settle();
    expect(stub.sent.at(-1)).toEqual({ method: "GET", path: "/api/v1/service-accounts/release-bot/tokens", body: undefined });
    const listed = p.all("service-token");
    expect(listed).toHaveLength(1);
    expect(listed[0].textContent).toContain("by @root");
    expect(listed[0].textContent).toContain("Never expires");

    type(p.target.querySelector("#service-token-label") as HTMLInputElement, "deploy");
    const tokenReads = () => stub.sent.filter((s) => s.path.endsWith("/tokens") && s.method === "GET").length;
    const before = tokenReads();
    submit(p.q("service-token-create"));
    await settle();
    await settle();
    expect(writes()).toEqual([
      { method: "POST", path: "/api/v1/service-accounts/release-bot/tokens", body: { name: "deploy" } },
    ]);
    expect(tokenReads()).toBe(before + 1);

    expect(p.q("service-token-text")?.textContent).toBe(SECRET);
    expect(p.q("service-token-issued")?.textContent).toContain("cannot be shown again");
    expect(p.q("service-token-issued")?.textContent).toContain("SPOV_SERVER_TOKEN");
    // Drawn once, and kept out of storage, the address and the console.
    expect(document.body.innerHTML.split(SECRET)).toHaveLength(2);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(location.href).not.toContain(SECRET);
    for (const spy of [log, warn, error]) {
      expect(JSON.stringify(spy.mock.calls)).not.toContain(SECRET);
    }

    // jsdom has no clipboard, so Copy says the text must be selected.
    (p.q("service-token-copy") as HTMLButtonElement).click();
    await settle();
    expect(p.q("service-token-copy")?.textContent).toContain("select the text");

    (p.q("service-token-done") as HTMLButtonElement).click();
    flushSync();
    expect(p.q("service-token-issued")).toBeNull();
    expect(document.body.innerHTML).not.toContain(SECRET);
    p.done();
  });

  it("forgets an issued token when another account's tokens are opened", async () => {
    const p = await mounted();
    (p.row("release-bot").querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
    await settle();
    type(p.target.querySelector("#service-token-label") as HTMLInputElement, "deploy");
    submit(p.q("service-token-create"));
    await settle();
    await settle();
    expect(document.body.innerHTML).toContain(SECRET);
    (p.row("alpha").querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
    await settle();
    expect(document.body.innerHTML).not.toContain(SECRET);
    // Opening the first account again does not bring it back.
    (p.row("release-bot").querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
    await settle();
    expect(document.body.innerHTML).not.toContain(SECRET);
    p.done();
  });

  it("revokes a token on the second press and reads its tokens again", async () => {
    const p = await mounted();
    (p.row("release-bot").querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
    await settle();
    (p.q("service-token-revoke") as HTMLButtonElement).click();
    flushSync();
    expect(writes()).toEqual([]);
    stub.tokens = [];
    (p.q("service-token-revoke-yes") as HTMLButtonElement).click();
    await settle();
    await settle();
    expect(writes()).toEqual([
      { method: "DELETE", path: "/api/v1/service-accounts/release-bot/tokens/01JCZ8Y3N5Q2W9V7T6R4P3M2K1", body: undefined },
    ]);
    expect(p.all("service-token")).toHaveLength(0);
    expect(p.q("service-tokens-none")).not.toBeNull();
    p.done();
  });

  it("offers Delete again on the row when a delete's off-boarding did not finish", async () => {
    const detail = "service account alpha is deleted, but not everything of its was ended: " +
      "1 stored credential not deleted; send the same delete again to retry";
    stub.next.push({ status: 500, body: { title: "Internal Server Error", status: 500, detail } });
    const p = await mounted();
    const row = p.row("alpha");
    (row.querySelector('[data-testid="service-account-delete"]') as HTMLButtonElement).click();
    flushSync();
    expect(row.textContent).toContain("revokes its tokens");
    stub.accounts = [release, gone, { ...alpha, disabled_at: "2026-11-17T00:00:00Z" }];
    (row.querySelector('[data-testid="service-account-delete-yes"]') as HTMLButtonElement).click();
    await settle();
    await settle();
    expect(p.q("service-account-row-failure")?.textContent).toContain("send the same delete again");
    expect(p.q("service-account-partial")?.textContent).toContain("alpha");
    const again = p.row("alpha").querySelector('[data-testid="service-account-delete-again"]') as HTMLButtonElement;
    expect(again).not.toBeNull();
    again.click();
    await settle();
    await settle();
    const deletes = writes().map((s) => `${s.method} ${s.path}`);
    expect(deletes).toEqual(["DELETE /api/v1/service-accounts/alpha", "DELETE /api/v1/service-accounts/alpha"]);
    expect(p.q("service-account-row-failure")).toBeNull();
    expect(p.q("service-account-partial")).toBeNull();
    p.done();
  });
});
