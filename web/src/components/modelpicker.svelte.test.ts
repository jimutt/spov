import { describe, expect, it } from "vitest";
import { flushSync, mount, unmount } from "svelte";
import ModelPicker from "./ModelPicker.svelte";
import type { ModelGroup } from "../lib/jobform";

/**
 * The picker mounted for its interaction: typing narrows the list,
 * choosing sets the value, and a custom name needs its own step.
 *
 * @decision(DL-061)
 */

const groups: ModelGroup[] = [
  {
    provider: "anthropic",
    name: "Anthropic",
    label: "Anthropic",
    available: true,
    models: ["anthropic/claude-opus-5", "anthropic/claude-sonnet-5"],
    beta: ["anthropic/claude-opus-5"],
  },
  { provider: "groq", name: "Groq", label: "Groq (needs GROQ_API_KEY)", available: false, models: ["groq/llama"] },
];

function draw(initial = "") {
  const target = document.createElement("div");
  document.body.append(target);
  let value = initial;
  const app = mount(ModelPicker, {
    target,
    props: {
      id: "m",
      groups,
      defaultLabel: "Your default",
      get value() {
        return value;
      },
      set value(v: string) {
        value = v;
      },
    },
  });
  flushSync();
  const q = (sel: string) => target.querySelector(sel) as HTMLElement | null;
  const options = () => Array.from(target.querySelectorAll('[data-testid="model-picker-option"]')) as HTMLButtonElement[];
  return {
    target,
    q,
    options,
    value: () => value,
    open: () => {
      (q('[data-testid="model-picker-value"]') as HTMLButtonElement).click();
      flushSync();
    },
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

function type(input: HTMLInputElement, text: string) {
  input.value = text;
  input.dispatchEvent(new Event("input", { bubbles: true }));
  flushSync();
}

describe("the model picker", () => {
  it("narrows as the reader types and sets the value on a choice", () => {
    const p = draw();
    p.open();
    expect(p.options().map((o) => o.textContent?.replace("beta", "").trim())).toEqual(["claude-opus-5", "claude-sonnet-5", "llama"]);
    expect(p.options()[2].disabled).toBe(true);
    type(p.q('[data-testid="model-picker-search"]') as HTMLInputElement, "sonnet");
    expect(p.options()).toHaveLength(1);
    p.options()[0].click();
    flushSync();
    expect(p.value()).toBe("anthropic/claude-sonnet-5");
    expect(p.q('[data-testid="model-picker-option"]')).toBeNull();
    p.done();
  });

  it("takes a model not listed only through its own entry, and says so", () => {
    const p = draw();
    p.open();
    const offer = Array.from(p.target.querySelectorAll("button")).find((b) => b.textContent?.includes("not listed"))!;
    offer.click();
    flushSync();
    const field = p.q('[data-testid="model-picker-custom-entry"] input') as HTMLInputElement;
    type(field, "claude-next");
    (Array.from(p.target.querySelectorAll("button")).find((b) => b.textContent === "Use") as HTMLButtonElement).click();
    flushSync();
    expect(p.value()).toBe("anthropic/claude-next");
    p.done();
    // A chosen model the list lacks carries the note.
    const chosen = draw("anthropic/claude-next");
    expect(chosen.q('[data-testid="model-picker-custom"]')?.textContent).toContain("Not in spov's model list");
    chosen.done();
  });
});
