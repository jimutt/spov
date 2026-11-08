import type { LogEvent } from "./api/live";

/**
 * What the page knows about a subagent from the parent's side: the call
 * that started it, which conversation it started, and how it is going.
 * Plain data in and out, so vitest reaches it; `buildTimelines` draws
 * the card from these.
 *
 * Parentage decides whether a conversation is a child (DL-111); the call
 * decides where its card is drawn (DL-112). The pairing never drops a
 * child for want of a link.
 *
 * @decision(DL-112) @decision(DL-110)
 */

/**
 * SubagentTools are the tool names that start a subagent: `subagent` at
 * the pinned build (spike S-SA1), `task` in OpenCode's earlier ones. A
 * call to any other tool is never a card.
 */
export const SubagentTools: ReadonlySet<string> = new Set(["subagent", "task"]);

/** How a subagent's card reads, as a state of the child's work. */
export type SubagentState = "starting" | "running" | "done" | "failed" | "stopped";

/**
 * One parent call to a subagent tool, joined from the frames of the
 * call. Positions are store positions, which order the log exactly where
 * timestamps would not.
 */
export interface DelegatedCall {
  call: string;
  /** Position of `tool.input.started`. */
  start: number;
  /** Time of the same frame, in ms; 0 when undated. */
  at: number;
  /** Position of the call's result, absent while it runs. */
  end?: number;
  state: "running" | "done" | "failed";
  /** The failure's text, when it failed. */
  error: string;
  /** The child its result names (`metadata.sessionID`), or the one a follow-up names in its input. */
  child: string;
  description: string;
  agent: string;
  background: boolean;
}

/**
 * Names a call's input carries. `sessionID` is a follow-up to an
 * existing child (spike S-SA1).
 */
function readInput(text: string): { description: string; agent: string; background: boolean; child: string } {
  const none = { description: "", agent: "", background: false, child: "" };
  if (!text.trim()) return none;
  try {
    const f: unknown = JSON.parse(text);
    if (!f || typeof f !== "object" || Array.isArray(f)) return none;
    const o = f as Record<string, unknown>;
    const str = (v: unknown) => (typeof v === "string" ? v.trim() : "");
    return {
      description: str(o.description),
      agent: str(o.agent) || str(o.subagent_type),
      background: o.background === true,
      child: str(o.sessionID),
    };
  } catch {
    return none;
  }
}

/**
 * delegatedCalls reads the calls to a subagent tool out of one
 * conversation's frames. The tool's name is only on `tool.input.started`,
 * so a call is known by that frame and joined to the rest by its id.
 */
export function delegatedCalls(activity: readonly LogEvent[]): DelegatedCall[] {
  const calls = new Map<string, DelegatedCall>();
  for (const ev of activity) {
    if (ev.source !== "opencode" || !ev.call || !ev.name.startsWith("session.tool.")) continue;
    if (ev.name === "session.tool.input.started") {
      if (ev.tool && SubagentTools.has(ev.tool) && !calls.has(ev.call)) {
        calls.set(ev.call, {
          call: ev.call,
          start: ev.pos,
          at: Date.parse(ev.at) || 0,
          state: "running",
          error: "",
          child: "",
          description: "",
          agent: "",
          background: false,
        });
      }
      continue;
    }
    const c = calls.get(ev.call);
    if (!c) continue;
    switch (ev.name) {
      case "session.tool.input.ended": {
        const input = readInput(ev.text ?? "");
        c.description = input.description;
        c.agent = input.agent;
        c.background = input.background;
        if (input.child && !c.child) c.child = input.child;
        break;
      }
      case "session.tool.success":
        c.state = "done";
        c.end = ev.pos;
        if (ev.child) c.child = ev.child;
        break;
      case "session.tool.failed":
        c.state = "failed";
        c.end = ev.pos;
        c.error = ev.text ?? "";
        break;
    }
  }
  return [...calls.values()];
}

/** A child conversation, as pairing sees it. */
export interface Spawned {
  id: string;
  /** Position of its first frame; Infinity while it has none. */
  first: number;
}

export interface Pairing {
  /** The child each call started, by call id. A call with none is absent. */
  byCall: Map<string, string>;
  /**
   * Calls that name a child another call already started: a follow-up
   * to it. They are not a card; the page keeps them as tool lines.
   */
  followUps: Set<string>;
  /** Children no call claimed, oldest first. */
  standalone: string[];
}

/**
 * pairSubagents decides which child each call started, in the three
 * steps of DL-112:
 *
 * 1. A call whose frames name a child that exists is paired with it
 *    (the normal case once the call has ended). A second call naming
 *    the same child is a follow-up, not another card.
 * 2. Every other call is paired with a child first seen during its
 *    span, from its start to its end (or without end while it runs),
 *    oldest to oldest, one each, so parallel calls take their children
 *    in the order they began. A call that named a child the page does
 *    not know yet waits for it rather than taking another's.
 * 3. A child no call claims stands alone: a background child that
 *    outlives its call, a plugin-created one.
 *
 * Pure: the same calls and children always pair the same way.
 */
export function pairSubagents(calls: readonly DelegatedCall[], children: readonly Spawned[]): Pairing {
  const known = new Set(children.map((c) => c.id));
  const byCall = new Map<string, string>();
  const followUps = new Set<string>();
  const claimed = new Set<string>();
  const ordered = [...calls].sort((a, b) => a.start - b.start);

  for (const c of ordered) {
    if (!c.child || !known.has(c.child)) continue;
    if (claimed.has(c.child)) followUps.add(c.call);
    else {
      byCall.set(c.call, c.child);
      claimed.add(c.child);
    }
  }

  const free = [...children].filter((c) => !claimed.has(c.id)).sort((a, b) => a.first - b.first || (a.id < b.id ? -1 : 1));
  for (const c of ordered) {
    // Named a child (known, or one still to be read): the name decides.
    if (c.child) continue;
    const to = c.end ?? Infinity;
    const at = free.findIndex((k) => k.first >= c.start && k.first <= to);
    if (at < 0) continue;
    byCall.set(c.call, free[at].id);
    claimed.add(free[at].id);
    free.splice(at, 1);
  }

  return { byCall, followUps, standalone: free.map((c) => c.id) };
}

/** The frames that open and close a turn (steer.ts's `TurnStarted`/`TurnEnders`; a test keeps them equal). */
const Started = "session.execution.started";
const Ended: Readonly<Record<string, SubagentState>> = {
  "session.execution.succeeded": "done",
  "session.execution.failed": "failed",
  "session.execution.interrupted": "stopped",
};

/**
 * subagentState says how a subagent is going. From the child's own
 * frames when it has any: its last execution frame, by position. Else
 * from the call that started it: a call still running is a card that is
 * starting, a failed one failed, and a finished foreground one done (a
 * background call finishes at once and says nothing of the child).
 * `ended` is the whole session having ended, which turns a child still
 * running into a stopped one.
 */
export function subagentState(
  child: readonly LogEvent[] | undefined,
  call: DelegatedCall | undefined,
  ended: boolean,
): SubagentState {
  let at = -1;
  let state: SubagentState | undefined;
  for (const ev of child ?? []) {
    if (ev.source !== "opencode" || ev.pos < at) continue;
    if (ev.name === Started) {
      at = ev.pos;
      state = "running";
    } else if (Ended[ev.name]) {
      at = ev.pos;
      state = Ended[ev.name];
    }
  }
  if (!state) {
    if (call?.state === "failed") state = "failed";
    else if (call?.state === "done" && !call.background) state = "done";
    else state = child?.length ? "running" : "starting";
  }
  return ended && (state === "running" || state === "starting") ? "stopped" : state;
}

/** The words a state is drawn with. */
export function stateWord(state: SubagentState): string {
  switch (state) {
    case "starting":
      return "Delegating…";
    case "running":
      return "Running";
    case "done":
      return "Done";
    case "failed":
      return "Failed";
    case "stopped":
      return "Stopped";
  }
}

/** The state's mark, as `StateMark` draws it. */
export function stateMark(state: SubagentState): "run" | "done" | "fail" | "stop" | "wait" {
  switch (state) {
    case "starting":
      return "wait";
    case "running":
      return "run";
    case "done":
      return "done";
    case "failed":
      return "fail";
    case "stopped":
      return "stop";
  }
}
