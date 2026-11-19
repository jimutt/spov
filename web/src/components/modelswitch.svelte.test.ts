import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The model switch, mounted, for the reasoning choice beside it: drawn
 * only when the session's sandbox lists modes for the model, Default
 * first; a choice of mode, of Default or of another model sends the
 * reference DL-161 says; and a refused switch, or opening the control
 * while nothing is read, asks the page to read the list again. The
 * logic is reasoning.test.ts's; the page's reads are
 * sessiondetail.svelte.test.ts's.
 *
 * @decision(DL-161)
 */

const stub = vi.hoisted(() => {
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": status >= 400 ? "application/problem+json" : "application/json" },
    });
  let switchStatus = 202;
  const sent: string[] = [];
  const fetched = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const path = new URL(req.url).pathname;
    if (path === "/api/v1/models") {
      return json({
        providers: [
          {
            provider: "anthropic",
            name: "Anthropic",
            have_key: true,
            key_env: "ANTHROPIC_API_KEY",
            models: ["anthropic/claude-opus-5", "anthropic/claude-sonnet-5"],
          },
          { provider: "openai", name: "OpenAI", have_key: true, key_env: "OPENAI_API_KEY", models: ["openai/gpt-5-nano"] },
        ],
      });
    }
    if (req.method === "POST" && path.endsWith("/model")) {
      const body = JSON.parse(await req.text()) as { model: string };
      sent.push(body.model);
      if (switchStatus !== 202) return json({ title: "high is not a reasoning mode", status: switchStatus }, switchStatus);
      return json({ model: body.model, conversation: "ses_1" }, 202);
    }
    return new Response(null, { status: 404 });
  });
  globalThis.fetch = fetched as unknown as typeof fetch;
  return {
    sent,
    refuse: (status: number) => {
      switchStatus = status;
    },
    reset: () => {
      switchStatus = 202;
      sent.length = 0;
    },
  };
});

import ModelSwitch from "./ModelSwitch.svelte";
import { reactiveProps } from "./reactiveprops.svelte";
import type { ModelVariants } from "../lib/reasoning";

beforeEach(() => stub.reset());

async function settle() {
  for (let i = 0; i < 30; i++) await Promise.resolve();
  flushSync();
}

const offered: ModelVariants = {
  read: true,
  models: {
    "anthropic/claude-opus-5": ["low", "medium", "high", "max"],
    "openai/gpt-5-nano": ["minimal", "low", "medium", "high"],
  },
};

async function draw(current: string, given: ModelVariants | null = offered) {
  const variants = given ?? undefined;
  const target = document.createElement("div");
  document.body.append(target);
  const stale = vi.fn();
  const props = reactiveProps({ ref: "ses_1", current, variants, onStale: stale });
  const app = mount(ModelSwitch, { target, props });
  await settle();
  const pick = <T extends Element>(id: string) => target.querySelector(`[data-testid="${id}"]`) as T | null;
  return {
    props,
    stale,
    reasoning: () => pick<HTMLSelectElement>("composer-reasoning"),
    choose: async (mode: string) => {
      const sel = pick<HTMLSelectElement>("composer-reasoning")!;
      sel.value = mode;
      sel.dispatchEvent(new Event("change", { bubbles: true }));
      await settle();
    },
    open: () => pick("model-switch")!.dispatchEvent(new Event("pointerdown", { bubbles: true })),
    pickModel: async (label: string) => {
      pick("model-switch")!.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      await settle();
      pick<HTMLButtonElement>("model-picker-value")!.click();
      flushSync();
      const option = Array.from(target.querySelectorAll('[data-testid="model-picker-option"]')).find(
        (o) => o.textContent?.trim() === label,
      ) as HTMLButtonElement;
      option.click();
      await settle();
    },
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("the reasoning choice", () => {
  it("is not drawn while the list is not read, or for a model without modes", async () => {
    const unread = await draw("anthropic/claude-opus-5", { read: false, models: {} });
    expect(unread.reasoning()).toBeNull();
    unread.done();
    const missing = await draw("anthropic/claude-opus-5", null);
    expect(missing.reasoning()).toBeNull();
    missing.done();

    const none = await draw("anthropic/claude-sonnet-5");
    expect(none.reasoning()).toBeNull();
    none.done();
  });

  it("offers Default, then the model's modes in the list's order, with the current one chosen", async () => {
    const c = await draw("anthropic/claude-opus-5#high");
    const options = Array.from(c.reasoning()!.options).map((o) => [o.value, o.textContent]);
    expect(options).toEqual([
      ["", "Default"],
      ["low", "low"],
      ["medium", "medium"],
      ["high", "high"],
      ["max", "max"],
    ]);
    expect(c.reasoning()!.value).toBe("high");
    c.done();
  });

  it("names a mode the list does not offer as the current one, and does not offer it", async () => {
    const c = await draw("anthropic/claude-opus-5#foo");
    const options = Array.from(c.reasoning()!.options);
    expect(options.map((o) => [o.value, o.textContent, o.disabled])).toEqual([
      ["", "Default", false],
      ["low", "low", false],
      ["medium", "medium", false],
      ["high", "high", false],
      ["max", "max", false],
      ["foo", "foo (not offered)", true],
    ]);
    expect(c.reasoning()!.value).toBe("foo");
    await c.choose("low");
    expect(stub.sent).toEqual(["anthropic/claude-opus-5#low"]);
    c.done();

    const offered = await draw("anthropic/claude-opus-5#high");
    expect(Array.from(offered.reasoning()!.options).some((o) => o.disabled)).toBe(false);
    offered.done();
  });

  it("switches at once to a mode, and to the model's default for Default", async () => {
    const c = await draw("anthropic/claude-opus-5#high");
    await c.choose("max");
    c.props.current = "anthropic/claude-opus-5#max";
    await settle();
    await c.choose("");
    expect(stub.sent).toEqual(["anthropic/claude-opus-5#max", "anthropic/claude-opus-5"]);
    c.done();
  });

  it("keeps the mode across a switch when the new model offers it, else goes back to Default", async () => {
    const c = await draw("anthropic/claude-opus-5#high");
    await c.pickModel("gpt-5-nano");
    expect(stub.sent).toEqual(["openai/gpt-5-nano#high"]);
    c.done();

    const d = await draw("anthropic/claude-opus-5#max");
    await d.pickModel("gpt-5-nano");
    expect(stub.sent).toEqual(["openai/gpt-5-nano#high", "openai/gpt-5-nano"]);
    d.done();
  });

  it("appears when the page's list arrives, and asks for it again on a refusal or an open while unread", async () => {
    const c = await draw("anthropic/claude-opus-5", { read: false, models: {} });
    expect(c.reasoning()).toBeNull();
    c.props.variants = offered;
    await settle();
    expect(c.reasoning()).not.toBeNull();
    expect(c.stale).not.toHaveBeenCalled();

    stub.refuse(422);
    await c.choose("high");
    expect(stub.sent).toEqual(["anthropic/claude-opus-5#high"]);
    expect(c.stale).toHaveBeenCalledTimes(1);
    expect(c.reasoning()!.value).toBe("");

    c.props.variants = { read: false, models: {} };
    await settle();
    c.open();
    await settle();
    expect(c.stale).toHaveBeenCalledTimes(2);
    c.done();
  });
});
