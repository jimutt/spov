import { describe, expect, it } from "vitest";
import { ApiError } from "./api/client";
import type { ModelGroup } from "./jobform";
import { canLaunchOn } from "./jobform";
import { CustomNote, currentModel, customProblem, customRef, filterGroups, isListed, pickNote, switchTo } from "./modelpick";
import type { Activity } from "./timeline";

const groups: ModelGroup[] = [
  {
    provider: "anthropic",
    name: "Anthropic",
    label: "Anthropic",
    available: true,
    models: ["anthropic/claude-opus-5", "anthropic/claude-sonnet-5"],
    beta: [],
  },
  {
    provider: "openrouter",
    name: "OpenRouter",
    label: "OpenRouter (needs OPENROUTER_API_KEY)",
    available: false,
    models: ["openrouter/qwen/qwen3-coder", "openrouter/anthropic/claude-sonnet-5"],
  },
];

// @decision(DL-061)
describe("filtering the model list", () => {
  it("keeps the models every term matches, across providers", () => {
    const got = filterGroups(groups, "sonnet");
    expect(got.map((g) => g.models)).toEqual([["anthropic/claude-sonnet-5"], ["openrouter/anthropic/claude-sonnet-5"]]);
    expect(filterGroups(groups, "open qwen").map((g) => g.models)).toEqual([["openrouter/qwen/qwen3-coder"]]);
    expect(filterGroups(groups, "ANTHROPIC")[0].models).toHaveLength(2);
    expect(filterGroups(groups, "nothing-like-it")).toEqual([]);
    expect(filterGroups(groups, "  ")).toBe(groups);
  });
});

// @decision(DL-061) @decision(DL-013)
describe("a model not listed", () => {
  it("is checked before it is sent", () => {
    expect(customProblem("anthropic", "")).toMatch(/Type/);
    expect(customProblem("anthropic", "a b")).toMatch(/no spaces/);
    expect(customProblem("anthropic", "m#high")).toMatch(/or #/);
    expect(customProblem("anthropic", "a\u202eb")).toMatch(/control/);
    expect(customProblem("anthropic", "/m")).toMatch(/slash/);
    expect(customProblem("anthropic", "anthropic/claude-x")).toMatch(/"anthropic\/" in front/);
    expect(customProblem("openrouter", "anthropic/claude-x")).toBe("");
    expect(customProblem("anthropic", "x".repeat(201))).toMatch(/at most/);
    expect(customProblem("anthropic", "é".repeat(101))).toMatch(/at most/);
    expect(customProblem("anthropic", " claude-next ")).toBe("");
    expect(customRef("anthropic", " claude-next ")).toBe("anthropic/claude-next");
  });

  it("is said to launch with a note only where it would", () => {
    expect(pickNote(groups, "anthropic/claude-next")).toEqual({ refused: false, text: CustomNote });
    expect(pickNote(groups, "openrouter/qwen/qwen9")?.refused).toBe(true);
    expect(pickNote(groups, "nosuch/x")?.refused).toBe(true);
    expect(pickNote(groups, "anthropic/claude-opus-5")).toBeNull();
    expect(pickNote(groups, "")).toBeNull();
  });

  it("launches only under a provider with a key", () => {
    expect(canLaunchOn(groups, "anthropic/claude-next")).toBe(true);
    expect(canLaunchOn(groups, "openrouter/qwen/qwen9")).toBe(false);
    expect(canLaunchOn(groups, "anthropic/")).toBe(false);
    expect(isListed(groups, "anthropic/claude-opus-5#high")).toBe(true);
    expect(isListed(groups, "anthropic/claude-next")).toBe(false);
  });
});

// @decision(DL-014)
describe("the session's model", () => {
  it("is the log's newest selection, else the launch's", () => {
    const ev = (model?: string) => ({ name: "session.model.selected", source: "opencode", model, at: "", pos: 1 }) as Activity;
    expect(currentModel([], "anthropic/claude-sonnet-5")).toBe("anthropic/claude-sonnet-5");
    expect(currentModel([ev("a/b"), ev(undefined), ev("c/d")], "x/y")).toBe("c/d");
    expect(currentModel([], undefined)).toBe("");
  });

  it("is switched through the server, which may refuse", async () => {
    let sent = "";
    await switchTo("a/b", async (m) => {
      sent = m;
      return { response: { ok: true, status: 202 } };
    });
    expect(sent).toBe("a/b");
    await expect(switchTo("a/b", async () => ({ response: { ok: false, status: 422 }, error: { status: 422 } }))).rejects.toBeInstanceOf(
      ApiError,
    );
  });
});
