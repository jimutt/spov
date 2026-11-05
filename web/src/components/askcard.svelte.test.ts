import { describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The question card, mounted for its bindings: choosing, checking and
 * Other reach the answer sent, and Dismiss sends the DELETE. The answer's
 * logic is asks.test.ts.
 *
 * @decision(DL-020)
 */

const stub = vi.hoisted(() => {
  const sent: { method: string; path: string; body: unknown }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const raw = await req.text();
    sent.push({ method: req.method, path: new URL(req.url).pathname, body: raw ? JSON.parse(raw) : undefined });
    return new Response(JSON.stringify({ id: "frm_1", conversation: "ses_1", decision: "answered" }), {
      status: 202,
      headers: { "content-type": "application/json" },
    });
  }) as typeof globalThis.fetch;
  return { sent };
});

import AskCard from "./AskCard.svelte";

const ask = {
  id: "frm_1",
  kind: "form" as const,
  title: "Questions",
  fields: [
    {
      key: "q0",
      type: "string",
      title: "Colour",
      options: [
        { value: "Red", label: "Red" },
        { value: "Blue", label: "Blue" },
      ],
      custom: true,
    },
    { key: "q1", type: "multiselect", title: "Notes", options: [{ value: "No", label: "No" }], custom: true },
  ],
};

async function settle() {
  for (let i = 0; i < 30; i++) await Promise.resolve();
  flushSync();
}

function draw() {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(AskCard, { target, props: { ref: "ses_1", ask, askState: { status: "pending" }, open: true } });
  flushSync();
  const input = (label: string) =>
    Array.from(target.querySelectorAll("label")).find((l) => l.textContent?.trim().startsWith(label))!
      .querySelector("input") as HTMLInputElement;
  return {
    target,
    pick: (label: string) => {
      input(label).click();
      flushSync();
    },
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("a question card", () => {
  it("sends the choices keyed by field, Other as its text", async () => {
    stub.sent.length = 0;
    const c = draw();
    const form = c.target.querySelector('[data-testid="ask-form"]') as HTMLFormElement;
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    flushSync();
    expect(c.target.querySelector('[data-testid="ask-problems"]')?.textContent).toContain("at least one");
    expect(stub.sent).toHaveLength(0);

    c.pick("Blue");
    c.pick("No");
    const others = Array.from(c.target.querySelectorAll("label")).filter((l) => l.textContent?.trim() === "Other");
    (others[1].querySelector("input") as HTMLInputElement).click();
    flushSync();
    const text = c.target.querySelector('input[aria-label="Other: Notes"]') as HTMLInputElement;
    text.value = "later";
    text.dispatchEvent(new Event("input"));
    flushSync();
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    await settle();
    expect(stub.sent).toEqual([
      { method: "POST", path: "/api/v1/sessions/ses_1/forms/frm_1", body: { answer: { q0: "Blue", q1: ["No", "later"] } } },
    ]);
    // Taken, not yet settled: the log says when it is.
    expect(c.target.querySelector('[data-testid="ask-sent"]')?.textContent).toContain("Waiting for the session");
    c.done();
  });

  it("dismisses with a DELETE", async () => {
    stub.sent.length = 0;
    const c = draw();
    (c.target.querySelector('[data-testid="ask-dismiss"]') as HTMLButtonElement).click();
    await settle();
    expect(stub.sent.map((s) => `${s.method} ${s.path}`)).toEqual(["DELETE /api/v1/sessions/ses_1/forms/frm_1"]);
    c.done();
  });
});
