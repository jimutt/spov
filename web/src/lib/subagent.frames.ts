import type { Activity } from "./timeline";

/**
 * Frames of a log in the shape the stream serves them, for the cases the
 * measured fixtures do not hold: parallel calls, a result that names no
 * child, a child under a child. Only the fields the page reads are set;
 * the shapes are the ones in `subagent.fixture.ts`.
 *
 * @decision(DL-112)
 */

let clock = Date.parse("2026-11-08T19:00:00Z");

/** A frame at the next position, `gap` seconds after the last. */
export function frame(session: string, pos: number, name: string, extra: Partial<Activity> = {}): Activity {
  clock += 1000;
  return { pos, name, session, source: "opencode", at: new Date(clock).toISOString(), ...extra };
}

/** The frames of a call to the subagent tool, up to the point it has reached. */
export function callFrames(
  session: string,
  call: string,
  at: number,
  o: { description?: string; agent?: string; background?: boolean; sessionID?: string; end?: "success" | "failed"; child?: string },
): Activity[] {
  const input: Record<string, unknown> = { agent: o.agent ?? "general", description: o.description ?? "", prompt: "do it" };
  if (o.background) input.background = true;
  if (o.sessionID) input.sessionID = o.sessionID;
  const out = [
    frame(session, at, "session.tool.input.started", { call, tool: "subagent" }),
    frame(session, at + 1, "session.tool.input.ended", { call, text: JSON.stringify(input) }),
    frame(session, at + 2, "session.tool.called", { call }),
  ];
  if (o.end === "success") out.push(frame(session, at + 3, "session.tool.success", { call, ...(o.child ? { child: o.child } : {}) }));
  if (o.end === "failed") out.push(frame(session, at + 3, "session.tool.failed", { call, text: "denied" }));
  return out;
}

/** What a child does first: the prompt enqueued, its turn started. */
export function childStart(session: string, at: number, prompt = "task"): Activity[] {
  return [
    frame(session, at, "session.inbox.enqueued", { role: "user", message: `msg_${session}`, text: prompt }),
    frame(session, at + 1, "session.execution.started"),
  ];
}

/** One tool call of a conversation, finished. */
export function toolFrames(session: string, call: string, at: number, tool: string, input: unknown): Activity[] {
  return [
    frame(session, at, "session.tool.input.started", { call, tool }),
    frame(session, at + 1, "session.tool.input.ended", { call, text: JSON.stringify(input) }),
    frame(session, at + 2, "session.tool.success", { call }),
  ];
}

/** A child's closing words and the end of its turn. */
export function childEnd(session: string, at: number, text: string, ending = "session.execution.succeeded"): Activity[] {
  return [frame(session, at, "session.text.ended", { text }), frame(session, at + 1, ending)];
}
