import { describe, expect, test } from "vitest";
import { commandLine, injectedLabel, shellEnd, shellFailed, shellWho } from "./injected";

// @decision(DL-180)
describe("the words for what OpenCode added", () => {
  test("a shell command's end", () => {
    expect(shellEnd({ command: "x", state: "completed", exit: 0 })).toBe("finished");
    expect(shellEnd({ command: "x", state: "completed" })).toBe("finished");
    expect(shellEnd({ command: "x", state: "completed", exit: 3 })).toBe("exit 3");
    expect(shellEnd({ command: "x", state: "cancelled" })).toBe("cancelled");
    expect(shellEnd({ command: "x", state: "error" })).toBe("failed to start");
    expect(shellFailed({ command: "x", exit: 1 })).toBe(true);
    expect(shellFailed({ command: "x", state: "error" })).toBe(true);
    expect(shellFailed({ command: "x", state: "cancelled" })).toBe(false);
  });

  test("who ran it and its first line", () => {
    expect(shellWho({ command: "x" })).toBe("Background command");
    expect(shellWho({ command: "x", by_person: true })).toBe("Run by a person");
    expect(commandLine("make\nmake test")).toBe("make …");
    expect(commandLine("  make  ")).toBe("make");
  });

  test("the fold's label", () => {
    expect(injectedLabel({ kind: "instructions", summary: "Loaded app/AGENTS.md" })).toBe("Loaded app/AGENTS.md");
    expect(injectedLabel({ kind: "other" })).toBe("Added by OpenCode or a plugin");
    expect(injectedLabel(undefined)).toBe("Added by OpenCode or a plugin");
  });
});
