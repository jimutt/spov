import { describe, expect, it } from "vitest";
import { formModes, keptMode, modeOf, modelChip, modesOf, switchedTo, withMode, type ModelVariants } from "./reasoning";

/**
 * The reasoning choice's logic: which modes a model offers in this
 * session, the reference a choice sends, and the chip's words.
 *
 * @decision(DL-161)
 */

const list: ModelVariants = {
  read: true,
  models: {
    "anthropic/claude-opus-5": ["low", "medium", "high", "max"],
    "openai/gpt-5-nano": ["minimal", "low", "medium", "high"],
    "openrouter/z-ai/glm-5": ["none", "thinking"],
  },
};

describe("the reasoning modes", () => {
  it("are the model's, in the list's order, whatever mode the reference names", () => {
    expect(modesOf(list, "anthropic/claude-opus-5")).toEqual(["low", "medium", "high", "max"]);
    expect(modesOf(list, "anthropic/claude-opus-5#high")).toEqual(["low", "medium", "high", "max"]);
    expect(modesOf(list, "openrouter/z-ai/glm-5#thinking")).toEqual(["none", "thinking"]);
  });

  it("are none for a model without any, an empty reference, or a list not read", () => {
    expect(modesOf(list, "mistral/small")).toEqual([]);
    expect(modesOf(list, "")).toEqual([]);
    expect(modesOf(undefined, "anthropic/claude-opus-5")).toEqual([]);
    expect(modesOf({ read: false, models: {} }, "anthropic/claude-opus-5")).toEqual([]);
    expect(modesOf({ read: true, models: { "a/b": null } }, "a/b")).toEqual([]);
  });

  it("are read off a reference and written onto one, Default being none", () => {
    expect(modeOf("anthropic/claude-opus-5#high")).toBe("high");
    expect(modeOf("anthropic/claude-opus-5")).toBe("");
    // An empty half is no variant, as withoutVariant has it.
    expect(modeOf("anthropic/claude-opus-5#")).toBe("");
    expect(withMode("anthropic/claude-opus-5#low", "max")).toBe("anthropic/claude-opus-5#max");
    expect(withMode("anthropic/claude-opus-5#low", "")).toBe("anthropic/claude-opus-5");
  });
});

describe("choosing another model", () => {
  it("keeps the mode when the new model offers one of that id", () => {
    expect(switchedTo(list, "anthropic/claude-opus-5#high", "openai/gpt-5-nano")).toBe("openai/gpt-5-nano#high");
    expect(switchedTo(list, "anthropic/claude-opus-5#low", "openai/gpt-5-nano#medium")).toBe("openai/gpt-5-nano#low");
  });

  it("goes back to Default when it does not, or nothing is read", () => {
    expect(switchedTo(list, "anthropic/claude-opus-5#max", "openai/gpt-5-nano")).toBe("openai/gpt-5-nano");
    expect(switchedTo(list, "anthropic/claude-opus-5#high", "mistral/small")).toBe("mistral/small");
    expect(switchedTo(undefined, "anthropic/claude-opus-5#high", "openai/gpt-5-nano")).toBe("openai/gpt-5-nano");
    expect(switchedTo(list, "anthropic/claude-opus-5", "openai/gpt-5-nano")).toBe("openai/gpt-5-nano");
  });
});

describe("the chip", () => {
  it("names the model and then its mode, and Default names nothing", () => {
    expect(modelChip("anthropic/claude-opus-5#high")).toBe("claude-opus-5 · high");
    expect(modelChip("anthropic/claude-opus-5")).toBe("claude-opus-5");
    expect(modelChip("openrouter/z-ai/glm-5#thinking")).toBe("glm-5 · thinking");
    expect(modelChip("")).toBe("");
  });
});

/**
 * Before a launch the modes come from the model list, and a choice of
 * another model keeps a mode only when that model offers it.
 *
 * @decision(DL-189)
 */
describe("a form's reasoning choice", () => {
  const providers = [
    { provider: "anthropic", name: "Anthropic", key_env: "ANTHROPIC_API_KEY", have_key: true,
      models: ["anthropic/a", "anthropic/b"], variants: { "anthropic/a": ["low", "high"] } },
    { provider: "openai", name: "OpenAI", key_env: "OPENAI_API_KEY", have_key: false, models: ["openai/c"] },
  ];
  it("reads the modes of the model, with or without a mode on it", () => {
    expect(formModes(providers, "anthropic/a")).toEqual(["low", "high"]);
    expect(formModes(providers, "anthropic/a#high")).toEqual(["low", "high"]);
    expect(formModes(providers, "anthropic/b")).toEqual([]);
    expect(formModes(providers, "openai/c")).toEqual([]);
    expect(formModes(providers, "")).toEqual([]);
    expect(formModes(undefined, "anthropic/a")).toEqual([]);
  });
  it("keeps a mode the new model offers, else goes back to Default", () => {
    expect(keptMode("high", ["low", "high"])).toBe("high");
    expect(keptMode("max", ["low", "high"])).toBe("");
    expect(keptMode("", ["low"])).toBe("");
  });
});
