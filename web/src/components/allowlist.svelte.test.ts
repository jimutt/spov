import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The People section mounted for its wiring: it draws the list, adds an
 * entry through PUT and shows whom it admitted, shows a refusal beside
 * the field, re-reads the list after every change, and offers Retry for
 * a removal whose off-boarding did not finish. The behaviour is in
 * ../lib/allowlist.test.ts.
 *
 * @decision(DL-129)
 */

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

const stub = vi.hoisted(() => {
  const state = {
    sent: [] as Sent[],
    entries: [] as unknown[],
    // The answer to the next write, else 200 with the entry or 204.
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
    const body = req.method === "PUT" ? JSON.parse(await req.text()) : undefined;
    state.sent.push({ method: req.method, path: url.pathname, body });
    if (req.method === "GET") return json(200, { entries: state.entries });
    const answer = state.next.shift();
    if (answer) return json(answer.status, answer.body);
    if (req.method === "DELETE") return new Response(null, { status: 204 });
    return json(200, {
      kind: "github", email: "", github_login: "Octo", github_id: 77, name: "Octo Cat",
      admin: false, you: false, created_at: "2026-11-01T00:00:00Z",
    });
  }) as typeof globalThis.fetch;
  return state;
});

import Allowlist from "./Allowlist.svelte";

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

const me = { kind: "email", email: "me@example.com", admin: true, you: true, created_at: "2026-11-01T00:00:00Z",
  last_login_at: "2026-11-10T09:00:00Z" };
const root = { kind: "github", email: "", github_login: "root", github_id: 1, admin: true, you: false,
  created_at: "2026-11-01T00:00:00Z" };
const bob = { kind: "github", email: "", github_login: "bob", github_id: 42, name: "<b>Bob</b>", admin: false,
  you: false, created_at: "2026-11-01T00:00:00Z", last_login_at: "2026-11-09T09:00:00Z" };
const never = { kind: "email", email: "new@example.com", admin: false, you: false, created_at: "2026-11-01T00:00:00Z",
  unbound: true };

async function mounted() {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(Allowlist, { target });
  await settle();
  return {
    target,
    q: (id: string) => target.querySelector(`[data-testid="${id}"]`),
    all: (id: string) => [...target.querySelectorAll(`[data-testid="${id}"]`)],
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

function gets() {
  return stub.sent.filter((s) => s.method === "GET").length;
}

describe("the People section", () => {
  beforeEach(() => {
    stub.sent.length = 0;
    stub.next.length = 0;
    stub.entries = [me, root, bob, never];
  });

  it("lists the entries as text, marks the caller, and guards their only admin entry", async () => {
    const p = await mounted();
    const rows = p.all("allowlist-entry");
    expect(rows.map((r) => r.querySelector("span")?.textContent)).toEqual([
      "me@example.com",
      "@root",
      "@bob",
      "new@example.com",
    ]);
    expect(p.all("allowlist-you")).toHaveLength(1);
    expect(rows[2].textContent).toContain("<b>Bob</b>");
    expect(rows[2].querySelector("b")).toBeNull();
    expect(rows[2].textContent).toContain("Last signed in");
    expect(rows[3].textContent).toContain("Never signed in");
    expect(rows[3].querySelector('[data-testid="allowlist-unbound"]')?.textContent).toContain("since accounts were added");
    expect(rows[2].querySelector('[data-testid="allowlist-unbound"]')).toBeNull();
    // The caller's only admin entry can be neither removed nor demoted.
    expect((rows[0].querySelector('[data-testid="allowlist-remove"]') as HTMLButtonElement).disabled).toBe(true);
    expect((rows[0].querySelector('[data-testid="allowlist-demote"]') as HTMLButtonElement).disabled).toBe(true);
    expect(rows[0].querySelector('[data-testid="allowlist-blocked"]')?.textContent).toContain("only administrator entry");
    p.done();
  });

  it("adds a GitHub username, shows whom GitHub resolved, and reads the list again", async () => {
    const p = await mounted();
    const before = gets();
    type(p.target.querySelector("#allowlist-entry") as HTMLInputElement, "@octo");
    (p.q("allowlist-add") as HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
    await settle();
    await settle();
    const put = stub.sent.find((s) => s.method === "PUT");
    expect(put).toEqual({ method: "PUT", path: "/api/v1/allowlist/%40octo", body: { admin: false } });
    expect(p.q("allowlist-added")?.textContent).toContain("Added @Octo, Octo Cat");
    expect((p.target.querySelector("#allowlist-entry") as HTMLInputElement).value).toBe("");
    expect(gets()).toBe(before + 1);
    p.done();
  });

  it("does not send a username GitHub could not have", async () => {
    const p = await mounted();
    type(p.target.querySelector("#allowlist-entry") as HTMLInputElement, "../users");
    expect(p.q("allowlist-add-note")?.textContent).toContain("is not a GitHub username");
    (p.q("allowlist-add") as HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
    await settle();
    expect(stub.sent.filter((s) => s.method !== "GET")).toEqual([]);
    p.done();
  });

  it("shows a refusal beside the field in the server's words", async () => {
    stub.next.push({
      status: 422,
      body: { title: "Unprocessable Entity", status: 422, detail: "GitHub knows no account named @nobody" },
    });
    const p = await mounted();
    const before = gets();
    type(p.target.querySelector("#allowlist-entry") as HTMLInputElement, "nobody");
    (p.q("allowlist-add") as HTMLFormElement).dispatchEvent(new Event("submit", { cancelable: true }));
    await settle();
    await settle();
    expect(p.q("allowlist-add-failure")?.textContent).toContain("GitHub knows no account named @nobody");
    expect((p.target.querySelector("#allowlist-entry") as HTMLInputElement).value).toBe("nobody");
    expect(gets()).toBe(before + 1);
    p.done();
  });

  it("offers Retry when the entry went but its off-boarding did not finish", async () => {
    const detail = "@bob (b@example.com) is removed from the allowlist, but not everything of theirs was ended: " +
      "1 stored credential not deleted; send the same delete again to retry";
    stub.next.push({ status: 500, body: { title: "Internal Server Error", status: 500, detail } });
    const p = await mounted();
    const row = p.all("allowlist-entry")[2];
    (row.querySelector('[data-testid="allowlist-remove"]') as HTMLButtonElement).click();
    flushSync();
    expect(row.textContent).toContain("revokes their CLI tokens");
    stub.entries = [me, root, never];
    (row.querySelector('[data-testid="allowlist-remove-yes"]') as HTMLButtonElement).click();
    await settle();
    await settle();
    expect(p.q("allowlist-row-failure")?.textContent).toContain("send the same delete again");
    expect(p.all("allowlist-entry")).toHaveLength(3);
    expect(p.q("allowlist-retry")?.textContent).toContain("@bob");
    (p.q("allowlist-retry")?.querySelector("button") as HTMLButtonElement).click();
    await settle();
    await settle();
    const deletes = stub.sent.filter((s) => s.method === "DELETE").map((s) => s.path);
    // By ID, both times: the label could be another entry's too.
    expect(deletes).toEqual(["/api/v1/allowlist/id%3A42", "/api/v1/allowlist/id%3A42"]);
    expect(p.q("allowlist-retry")).toBeNull();
    expect(p.q("allowlist-row-failure")).toBeNull();
    p.done();
  });
});
