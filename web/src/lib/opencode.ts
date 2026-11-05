import type { components } from "./api/types";

/**
 * The Settings page's OpenCode Console sign-in, kept out of the component
 * so a test can reach it. The server runs the device flow; the browser
 * only shows the code and asks how the attempt stands.
 *
 * @decision(DL-015)
 */

export type Connection = components["schemas"]["OpenCodeConnectionView"];
export type Attempt = components["schemas"]["OpenCodeAttemptView"];

/**
 * waitForSignIn asks about an attempt every `interval` seconds until it
 * is no longer pending, and returns how it ended. An aborted wait throws
 * the signal's reason, so a page that is left stops asking.
 *
 * @decision(DL-015)
 */
export async function waitForSignIn(
  first: Attempt,
  check: (id: string) => Promise<Attempt>,
  sleep: (ms: number, signal: AbortSignal) => Promise<void>,
  signal: AbortSignal,
): Promise<Attempt> {
  let a = first;
  const every = Math.max(a.interval, 1) * 1000;
  while (a.state === "pending") {
    await sleep(every, signal);
    signal.throwIfAborted();
    a = await check(a.id);
  }
  return a;
}

/** sleep resolves after ms, or rejects when signal aborts. */
export function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}

/**
 * connectionLine says where the caller's sign-in stands, in one sentence.
 *
 * @decision(DL-015)
 */
export function connectionLine(c: Connection | undefined): string {
  if (!c) return "";
  if (!c.enabled) {
    return "This server cannot hold an OpenCode sign-in: it has nowhere safe to keep one.";
  }
  if (c.needs_reconnect) {
    return "OpenCode refused the stored sign-in, so sessions run without it. Connect again.";
  }
  if (!c.connected) {
    return "Not connected. Sessions can still use a pasted OPENCODE_API_KEY.";
  }
  return c.org_id
    ? `Connected to OpenCode (organization ${c.org_id}).`
    : "Connected to OpenCode.";
}

/**
 * attemptLine words how a finished attempt ended.
 *
 * @decision(DL-015)
 */
export function attemptLine(a: Attempt): string {
  switch (a.state) {
    case "connected":
      return "Connected. Sessions can now use the opencode-go/ and opencode/ models.";
    case "expired":
      return "The code expired before it was approved. Start again.";
    case "failed":
      return `The sign-in did not finish: ${a.reason ?? "no reason given"}.`;
    default:
      return "";
  }
}
