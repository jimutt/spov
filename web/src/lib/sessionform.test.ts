import { describe, expect, it } from "vitest";
import { Committed } from "./jobform";
import { sessionProblems, startAndGo, toSessionRequest } from "./sessionform";
import { ApiError } from "./api/client";

const repo = { platform: "github", owner: "o", name: "r" };

// @decision(DL-061)
describe("the session form", () => {
  it("needs a repository and sends nothing else", () => {
    expect(sessionProblems([])).toEqual(["choose at least one repository"]);
    expect(sessionProblems([repo])).toEqual([]);
    expect(toSessionRequest([repo])).toEqual({ repos: [repo] });
  });

  it("goes to the new session's page", async () => {
    let went = "";
    let sent: unknown;
    await startAndGo(
      [repo],
      async (body) => {
        sent = body;
        return { response: { ok: true, status: 202 }, data: { id: "01ABC" } };
      },
      (url) => (went = url),
    );
    expect(sent).toEqual({ repos: [repo] });
    expect(went).toBe("/session/?id=01ABC");
  });

  it("throws a refusal as ApiError, and a session it cannot follow as Committed", async () => {
    await expect(
      startAndGo([repo], async () => ({ response: { ok: false, status: 429 }, error: { status: 429 } }), () => {}),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(startAndGo([repo], async () => ({ response: { ok: true, status: 202 }, data: {} }), () => {})).rejects.toBeInstanceOf(
      Committed,
    );
    await expect(
      startAndGo(
        [repo],
        async () => ({ response: { ok: true, status: 202 }, data: { id: "x" } }),
        () => {
          throw new Error("no");
        },
      ),
    ).rejects.toBeInstanceOf(Committed);
  });
});

// A model is sent only when one was chosen.
//
// @decision(DL-061)
describe("the session form's model", () => {
  it("is left out unless chosen", () => {
    expect(toSessionRequest([repo], "  ")).toEqual({ repos: [repo] });
    expect(toSessionRequest([repo], "anthropic/claude-x")).toEqual({ repos: [repo], model: "anthropic/claude-x" });
  });
});
