import { describe, expect, it } from "vitest";
import {
  attemptLine,
  connectionLine,
  waitForSignIn,
  type Attempt,
} from "./opencode";

const attempt = (
  state: Attempt["state"],
  extra: Partial<Attempt> = {},
): Attempt => ({
  id: "oca_1",
  state,
  verification_url: "https://opencode.ai/console/device?user_code=AB-CD",
  user_code: "AB-CD",
  expires_at: "2026-10-03T12:10:00Z",
  interval: 5,
  ...extra,
});

describe("waitForSignIn", () => {
  it("asks at the server's interval until the attempt ends", async () => {
    const answers: Attempt[] = [attempt("pending"), attempt("connected")];
    const waited: number[] = [];
    const got = await waitForSignIn(
      attempt("pending"),
      async () => answers.shift()!,
      async (ms) => {
        waited.push(ms);
      },
      new AbortController().signal,
    );
    expect(got.state).toBe("connected");
    expect(waited).toEqual([5000, 5000]);
  });

  it("stops asking once the page is left", async () => {
    const ctl = new AbortController();
    let checks = 0;
    const wait = waitForSignIn(
      attempt("pending"),
      async () => {
        checks++;
        return attempt("pending");
      },
      async () => {
        ctl.abort(new Error("left"));
      },
      ctl.signal,
    );
    await expect(wait).rejects.toThrow("left");
    expect(checks).toBe(0);
  });
});

describe("connectionLine", () => {
  it("says what the server can hold and what the caller has", () => {
    expect(connectionLine(undefined)).toBe("");
    expect(connectionLine({ enabled: false, connected: false })).toMatch(
      /cannot hold/,
    );
    expect(connectionLine({ enabled: true, connected: false })).toMatch(
      /Not connected/,
    );
    expect(
      connectionLine({
        enabled: true,
        connected: false,
        needs_reconnect: true,
      }),
    ).toMatch(/Connect again/);
    expect(
      connectionLine({ enabled: true, connected: true, org_id: "org_1" }),
    ).toBe("Connected to OpenCode (organization org_1).");
  });
});

describe("attemptLine", () => {
  it("words each ending", () => {
    expect(attemptLine(attempt("connected"))).toMatch(/opencode-go/);
    expect(attemptLine(attempt("expired"))).toMatch(/expired/);
    expect(
      attemptLine(
        attempt("failed", { reason: "the sign-in was denied in the browser" }),
      ),
    ).toBe(
      "The sign-in did not finish: the sign-in was denied in the browser.",
    );
    expect(attemptLine(attempt("pending"))).toBe("");
  });
});
