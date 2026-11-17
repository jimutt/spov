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
    // The status of the account listing: 501 is a local server.
    listStatus: 200,
    // The answer to the next write, else a default success.
    next: [] as { status: number; body?: unknown }[],
    // Requests to hold until released, by method and path ending.
    holds: [] as { method: string; ends: string }[],
    // The held requests' releases, in arrival order. A release with no
    // answer gives the stub's answer at the time of release.
    waiting: [] as ((over?: { status: number; body?: unknown }) => void)[],
  };
  const json = (status: number, body: unknown) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": status < 300 ? "application/json" : "application/problem+json" },
    });
  const answer = (method: string, path: string): Response => {
    if (method === "GET") {
      if (path.endsWith("/tokens")) return json(200, { tokens: state.tokens });
      if (state.listStatus !== 200) {
        return json(state.listStatus, { title: "Not Implemented", status: state.listStatus, detail: "local" });
      }
      return json(200, { accounts: state.accounts });
    }
    const next = state.next.shift();
    if (next) return json(next.status, next.body);
    if (method === "DELETE") return new Response(null, { status: 204 });
    if (path.endsWith("/tokens")) {
      return json(201, {
        id: "01JCZ8Y3N5Q2W9V7T6R4P3M2K9", name: "deploy", created_at: "2026-11-17T00:00:00Z",
        expires_at: "2027-11-17T00:00:00Z", issued_by: { id: "u1", label: "@root" }, token: "spov_ThisIsTheIssuedTokenText0123456789",
      });
    }
    return json(201, {
      id: "01JCZ8Y3N5Q2W9V7T6R4P3M2KA", name: "new-bot", git_name: "new-bot", git_email: "new-bot@service.invalid",
      created_at: "2026-11-17T00:00:00Z", tokens: 0,
    });
  };
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    const text = req.method === "POST" || req.method === "PATCH" ? await req.text() : "";
    state.sent.push({ method: req.method, path: url.pathname, body: text ? JSON.parse(text) : undefined });
    const h = state.holds.findIndex((x) => x.method === req.method && url.pathname.endsWith(x.ends));
    if (h >= 0) {
      state.holds.splice(h, 1);
      return new Promise<Response>((resolve) =>
        state.waiting.push((over) => resolve(over ? json(over.status, over.body) : answer(req.method, url.pathname))),
      );
    }
    return answer(req.method, url.pathname);
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

function toggle(p: Awaited<ReturnType<typeof mounted>>, name: string) {
  (p.row(name).querySelector('[data-testid="service-account-tokens-toggle"]') as HTMLButtonElement).click();
  flushSync();
}

async function issueToken(p: Awaited<ReturnType<typeof mounted>>, label = "deploy") {
  type(p.target.querySelector("#service-token-label") as HTMLInputElement, label);
  submit(p.q("service-token-create"));
  await settle();
  await settle();
}

const buttons = (p: Awaited<ReturnType<typeof mounted>>, id: string) => p.all(id) as HTMLButtonElement[];

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
    stub.listStatus = 200;
    stub.holds.length = 0;
    stub.waiting.length = 0;
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
    await settle();
    expect(document.activeElement?.id).toBe("service-token-label");
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
  it("keeps a token issued while the tokens were toggled, and shows it under its account", async () => {
    const p = await mounted();
    toggle(p, "release-bot");
    await settle();
    stub.holds.push({ method: "POST", ends: "/tokens" });
    type(p.target.querySelector("#service-token-label") as HTMLInputElement, "deploy");
    submit(p.q("service-token-create"));
    await settle();
    expect(buttons(p, "service-account-tokens-toggle").every((b) => b.disabled)).toBe(true);
    toggle(p, "alpha");
    toggle(p, "release-bot");
    stub.waiting.shift()!();
    await settle();
    await settle();
    const panel = p.q("service-token-issued");
    expect(panel?.closest('[data-testid="service-account"]')).toBe(p.row("release-bot"));
    expect(p.q("service-token-text")?.textContent).toBe(SECRET);
    p.done();
  });

  it("lets nothing hide a shown token but Done", async () => {
    const p = await mounted();
    toggle(p, "release-bot");
    await settle();
    await issueToken(p);
    expect(p.q("service-token-issued")?.textContent).toContain("Press Done before doing anything else here.");
    expect(p.q("service-token-issued")?.textContent).toContain("until you press Done or leave the page");
    const blocked = [
      ...buttons(p, "service-account-tokens-toggle"),
      ...buttons(p, "service-account-edit-open"),
      ...buttons(p, "service-account-delete"),
      ...buttons(p, "service-account-delete-again"),
      ...buttons(p, "service-token-revoke"),
      p.q("service-account-create")?.querySelector("button[type=submit]") as HTMLButtonElement,
      p.q("service-token-create")?.querySelector("button[type=submit]") as HTMLButtonElement,
    ];
    expect(blocked.length).toBeGreaterThan(7);
    for (const b of blocked) {
      expect(b.disabled, b.textContent ?? "").toBe(true);
      b.click();
      flushSync();
    }
    submit(p.q("service-token-create"));
    submit(p.q("service-account-create"));
    await settle();
    expect(p.q("service-token-text")?.textContent).toBe(SECRET);
    expect(p.q("service-account-delete-ask")).toBeNull();
    expect(writes().filter((w) => w.method !== "POST" || !w.path.endsWith("/tokens"))).toEqual([]);
    p.done();
  });

  it("forgets a shown token when the page is hidden for good", async () => {
    const p = await mounted();
    toggle(p, "release-bot");
    await settle();
    await issueToken(p);
    expect(document.body.innerHTML).toContain(SECRET);
    window.dispatchEvent(new Event("pagehide"));
    flushSync();
    expect(document.body.innerHTML).not.toContain(SECRET);
    p.done();
  });

  it("shows only the refusal when a 500 leaves the account undeleted", async () => {
    stub.next.push({ status: 500, body: { title: "Internal Server Error", status: 500, detail: "store: broken" } });
    const p = await mounted();
    (p.row("alpha").querySelector('[data-testid="service-account-delete"]') as HTMLButtonElement).click();
    flushSync();
    (p.row("alpha").querySelector('[data-testid="service-account-delete-yes"]') as HTMLButtonElement).click();
    await settle();
    await settle();
    expect(p.q("service-account-row-failure")?.textContent).toContain("store: broken");
    expect(p.q("service-account-partial")).toBeNull();
    expect(p.row("alpha").querySelector('[data-testid="service-account-delete-again"]')).toBeNull();
    p.done();
  });

  it("drops a shown token and its tokens when a re-read finds the account deleted", async () => {
    const p = await mounted();
    toggle(p, "release-bot");
    await settle();
    stub.holds.push({ method: "POST", ends: "/tokens" });
    type(p.target.querySelector("#service-token-label") as HTMLInputElement, "deploy");
    submit(p.q("service-token-create"));
    await settle();
    stub.accounts = [{ ...release, disabled_at: "2026-11-17T00:00:00Z" }, gone, alpha];
    stub.waiting.shift()!();
    await settle();
    await settle();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(p.q("service-account-tokens")).toBeNull();
    p.done();
  });

  it("closes an author edit when a re-read finds the account deleted", async () => {
    const p = await mounted();
    (p.row("alpha").querySelector('[data-testid="service-account-edit-open"]') as HTMLButtonElement).click();
    flushSync();
    type(p.target.querySelector("#service-account-edit-name") as HTMLInputElement, "Alpha");
    stub.next.push({ status: 409, body: { title: "Conflict", status: 409, detail: "the service account alpha is deleted" } });
    stub.accounts = [release, gone, { ...alpha, disabled_at: "2026-11-17T00:00:00Z" }];
    submit(p.q("service-account-edit"));
    await settle();
    await settle();
    expect(p.q("service-account-edit")).toBeNull();
    expect(p.row("alpha").querySelector('[data-testid="service-account-delete-again"]')).not.toBeNull();
    p.done();
  });

  it("opens no confirmation while another act is in flight, and says Working only on that act", async () => {
    const p = await mounted();
    stub.holds.push({ method: "POST", ends: "/service-accounts" });
    type(p.target.querySelector("#service-account-name") as HTMLInputElement, "new-bot");
    submit(p.q("service-account-create"));
    await settle();
    const create = p.q("service-account-create")?.querySelector("button[type=submit]") as HTMLButtonElement;
    expect(create.textContent).toBe("Working…");
    const deletes = buttons(p, "service-account-delete");
    expect(deletes.every((b) => b.disabled)).toBe(true);
    deletes[0].click();
    flushSync();
    expect(p.q("service-account-delete-ask")).toBeNull();
    const again = p.q("service-account-delete-again") as HTMLButtonElement;
    expect(again.disabled).toBe(true);
    expect(again.textContent).toBe("Delete again");
    stub.waiting.shift()!();
    await settle();
    await settle();
    expect(create.textContent).toBe("Create");
    p.done();
  });

  it("never lets an older token read replace a newer one", async () => {
    const p = await mounted();
    stub.holds.push({ method: "GET", ends: "/tokens" }, { method: "GET", ends: "/tokens" });
    toggle(p, "release-bot");
    toggle(p, "release-bot");
    toggle(p, "release-bot");
    await settle();
    expect(stub.waiting).toHaveLength(2);
    const [older, newer] = stub.waiting.splice(0, 2);
    newer({ status: 200, body: { tokens: [{ ...ciToken, name: "newer" }] } });
    await settle();
    older({ status: 200, body: { tokens: [{ ...ciToken, name: "older" }] } });
    await settle();
    expect(p.all("service-token").map((t) => t.querySelector("span")?.textContent)).toEqual(["newer"]);
    p.done();
  });

  it("names each row's buttons after their account, and moves focus to a new token", async () => {
    const p = await mounted();
    const name = (id: string, row: string) =>
      p.row(row).querySelector(`[data-testid="${id}"]`)?.getAttribute("aria-label");
    expect(name("service-account-tokens-toggle", "release-bot")).toBe("Tokens for release-bot");
    expect(name("service-account-edit-open", "release-bot")).toBe("Change author of release-bot");
    expect(name("service-account-delete", "release-bot")).toBe("Delete release-bot");
    expect(name("service-account-delete-again", "aaa-old")).toBe("Delete aaa-old again");
    expect(p.row("release-bot").querySelector('[data-testid="service-account-tokens-toggle"]')?.textContent).toBe("Tokens");
    toggle(p, "release-bot");
    await settle();
    expect(name("service-token-revoke", "release-bot")).toBe("Revoke token ci of release-bot");
    await issueToken(p);
    expect(p.q("service-token-issued")?.contains(document.activeElement)).toBe(true);
    p.done();
  });

  it("draws nothing on a server with no service accounts", async () => {
    stub.listStatus = 501;
    const p = await mounted();
    expect(p.target.querySelector("h2")).toBeNull();
    expect(p.target.querySelector("form")).toBeNull();
    p.done();
  });

  it("sends the lifetime chosen", async () => {
    const p = await mounted();
    toggle(p, "release-bot");
    await settle();
    const choose = (v: string) => {
      const sel = p.target.querySelector("#service-token-lifetime") as HTMLSelectElement;
      sel.value = v;
      sel.dispatchEvent(new Event("change", { bubbles: true }));
      flushSync();
    };
    choose("days");
    type(p.target.querySelector("#service-token-days") as HTMLInputElement, "30");
    await issueToken(p, "short");
    (p.q("service-token-done") as HTMLButtonElement).click();
    await settle();
    choose("never");
    await issueToken(p, "forever");
    expect(writes().map((w) => w.body)).toEqual([
      { name: "short", expires_in_days: 30 },
      { name: "forever", no_expiry: true },
    ]);
    p.done();
  });

  it("copies the token to the clipboard", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    try {
      const p = await mounted();
      toggle(p, "release-bot");
      await settle();
      await issueToken(p);
      (p.q("service-token-copy") as HTMLButtonElement).click();
      await settle();
      expect(writeText).toHaveBeenCalledWith(SECRET);
      expect(p.q("service-token-copy")?.textContent).toBe("Copied");
      p.done();
    } finally {
      delete (navigator as { clipboard?: unknown }).clipboard;
    }
  });

  it("sends one create for an Enter and a click while it is pending", async () => {
    const p = await mounted();
    stub.holds.push({ method: "POST", ends: "/service-accounts" });
    type(p.target.querySelector("#service-account-name") as HTMLInputElement, "new-bot");
    submit(p.q("service-account-create"));
    await settle();
    submit(p.q("service-account-create"));
    (p.q("service-account-create")?.querySelector("button[type=submit]") as HTMLButtonElement).click();
    await settle();
    expect(writes()).toHaveLength(1);
    stub.waiting.shift()!();
    await settle();
    p.done();
  });
});
