import type { LogEvent } from "./api/live";
import { AskOpened, isAskFrame, type Ask } from "./asks";

/**
 * The session page's timeline: the recorded conversation and the event
 * log, laid on one chronological list. Both come from the store, so
 * both survive the sandbox they were recorded from. Plain data in,
 * plain data out, so it is testable outside the component.
 *
 * @decision(DL-058)
 */

export type Message = {
  /**
   * OpenCode's own message id (msg_…), carried so a client can
   * recognise a message it has already drawn. Empty for a message
   * stored without one.
   *
   * @decision(DL-019) @decision(DL-059)
   */
  id?: string;
  role?: string;
  text?: string;
  at?: string;
};
/**
 * `parent` is the OpenCode id of the conversation that started this one as
 * a subagent, absent for a root.
 *
 * @decision(DL-111) @decision(DL-110)
 */
export type Conversation = { id: string; title?: string; parent?: string; messages?: Message[] };

/**
 * childIds is the set of conversations that are a subagent's. A session
 * named by no conversation here is not in it, so it counts as a root.
 *
 * @decision(DL-111)
 */
export function childIds(conversations: readonly Conversation[]): ReadonlySet<string> {
  return new Set(conversations.filter((c) => c.parent).map((c) => c.id));
}

/**
 * One frame of the log. It is the stream's event unchanged: the store
 * numbers it, so the page no longer needs an arrival counter of its own.
 *
 * @decision(DL-058)
 */
export type Activity = LogEvent;

/**
 * A seam is one of spov's own markers — where the recorder was not
 * listening, or gave up. Decided by `source`, never by the name: a
 * sandbox chooses event names, and keying on a `spov.` prefix would let
 * it draw a seam of its own wording. The server refuses to store an
 * OpenCode-sourced row claiming `source: "spov"`.
 *
 * @decision(DL-058) @decision(DL-017)
 */
export function isSeam(ev: Activity): boolean {
  return ev.source === "spov";
}

/**
 * Markers spov writes for the audit trail rather than for the reader:
 * who sent a message or ended a turn, as machine identifiers, above a
 * transcript that already carries the words themselves.
 *
 * Not what `isSeam` is keyed on — `source` decides that, since a name
 * is a sandbox's to choose. This is a narrower question asked only of
 * rows already known to be spov's: which describe the recording (shown
 * always) and which are this audit trail (hidden here, shown elsewhere
 * as raw rows if this build doesn't recognise them).
 *
 * @decision(DL-059) @decision(DL-019) @decision(DL-053) @decision(DL-054) @decision(DL-020) @decision(DL-014)
 */
const AuditMarkers = new Set<string>([
  "spov.session.steered",
  "spov.session.message.cancelled",
  "spov.session.permission.replied",
  "spov.session.form.answered",
  "spov.session.form.dismissed",
  "spov.session.model.switched",
  "spov.job.interrupted",
  "spov.job.resumed",
  "spov.job.answered",
]);

/**
 * isAudit reports whether this is one of spov's own rows that belongs
 * in the audit trail rather than in the conversation.
 *
 * @decision(DL-059)
 */
export function isAudit(ev: Activity): boolean {
  return isSeam(ev) && AuditMarkers.has(ev.name ?? "");
}

/**
 * Markers that carry a record for the supervisor to judge from — the
 * plugin's evidence and outcome lines, stored whole. The verdict in the
 * sidebar is what they are read into, so the transcript does not draw
 * them. Asked only of spov's own rows, as `AuditMarkers` is.
 *
 * @decision(DL-058) @decision(DL-051)
 */
const RecordMarkers = new Set<string>(["spov.job.evidence", "spov.job.outcome"]);

/**
 * isRecord reports whether this is one of spov's own rows that holds a
 * record for the verdict rather than anything said to the reader.
 *
 * @decision(DL-058)
 */
export function isRecord(ev: Activity): boolean {
  return isSeam(ev) && RecordMarkers.has(ev.name ?? "");
}

/**
 * Markers that say what spov did with the job rather than that the
 * recording is incomplete: drawn as one of spov's quiet lines, like the
 * stop marker, not across the column as a gap is. Every other marker of
 * spov's stays a seam, including one this build has not heard of.
 *
 * @decision(DL-058) @decision(DL-013) @decision(DL-021)
 */
const NoteMarkers = new Set<string>([
  "spov.job.collected",
  "spov.job.bounded",
  "spov.session.model.custom",
  "spov.session.credentials.renewal.failed",
]);

/**
 * SeamTextLimit bounds how much of a seam's text is drawn, in
 * characters. A seam is normally one short sentence but `spov.job.evidence`
 * carries the plugin's whole evidence file, which must not print as
 * unbounded prose.
 *
 * @decision(DL-058)
 */
export const SeamTextLimit = 120;

/**
 * seamText is what a seam actually shows: its text if short enough to
 * read, otherwise the marker's name rather than a truncated head — a
 * head of JSONL reads as though it were the whole payload.
 *
 * @decision(DL-058)
 */
export function seamText(ev: { name?: string; text?: string }): string {
  const text = (ev.text ?? "").trim();
  const name = ev.name ?? "";
  if (!text) return name;
  if (text.length > SeamTextLimit || text.includes("\n")) return name || text.slice(0, SeamTextLimit);
  return text;
}

/**
 * NoteTextLimit bounds a quiet note's text, in characters. A note's
 * text is spov's own sentence, never a payload, so it is longer than a
 * seam's and a long one is shortened rather than replaced by its name.
 *
 * @decision(DL-021)
 */
export const NoteTextLimit = 400;

/**
 * noteText is what a quiet note shows: its text on one line, shortened
 * past NoteTextLimit, or the marker's name when it has none.
 *
 * @decision(DL-021)
 */
export function noteText(ev: { name?: string; text?: string }): string {
  const text = (ev.text ?? "").replace(/\s*\n\s*/g, " ").trim();
  if (!text) return ev.name ?? "";
  return text.length > NoteTextLimit ? text.slice(0, NoteTextLimit - 1).trimEnd() + "…" : text;
}

/**
 * shownWarnings drops a transcript warning once anything is on screen.
 * A warning describes the read that produced it and would otherwise
 * outlive that read as entries keep arriving on the live stream —
 * derived rather than cleared, so a later re-read that finds the same
 * thing again says so again.
 *
 * @decision(DL-017)
 */
export function shownWarnings(warnings: string[], entries: number): string[] {
  return entries > 0 ? [] : warnings;
}

/**
 * The marker the plugin says into the session when its loop stops. A
 * fourth place this literal is written, after the Go constant, the
 * JavaScript one and the supervisor's matcher; must not be reworded.
 *
 * @decision(DL-058) @decision(DL-048)
 */
const StopMarker = "SPOV_LOOP_STOPPED";

/**
 * stopNote turns the stop marker into a sentence, or returns null when
 * the text is not one. Fails soft: an unreadable payload returns null
 * and the caller shows the raw line rather than losing the event.
 *
 * @decision(DL-058) @decision(DL-048)
 */
/** TurnLimit is the largest turn count worth believing. */
const TurnLimit = 100_000;

/** ReasonLimit bounds an unrecognised reason, which a sandbox writes. */
const ReasonLimit = 60;

function clampReason(reason: string): string {
  return reason.length <= ReasonLimit ? reason : `${reason.slice(0, ReasonLimit)}…`;
}

export function stopNote(text: string | undefined): string | null {
  const body = (text ?? "").trim();
  if (!body.startsWith(StopMarker)) return null;
  let payload: Record<string, unknown> = {};
  const brace = body.indexOf("{");
  if (brace >= 0) {
    try {
      payload = JSON.parse(body.slice(brace)) as Record<string, unknown>;
    } catch {
      // Fails soft: show the raw line rather than lose the marker.
      return null;
    }
  }
  const reason = typeof payload.reason === "string" ? payload.reason : "";
  // The reasons the plugin actually sends. A pause announces
  // nothing (DL-039), so there is no wording for it here.
  //
  // Object.create(null), not an object literal: the reason comes from a
  // sandbox, and `said["constructor"]` on a plain object would return a
  // function rather than undefined.
  const said: Record<string, string> = Object.assign(Object.create(null), {
    declared: "the agent declared the goal met",
    stopped: "a person stopped the loop",
    "continue-failed": "the loop could not be continued",
    // @decision(DL-049)
    "no-completion-tool": "the completion tool could not be registered",
    // @decision(DL-049)
    "wall-clock": "the job's wall clock ran out",
    // @decision(DL-040)
    denied: "the same call kept being denied",
  });
  const why = said[reason] ?? (reason ? `reason: ${clampReason(reason)}` : "");
  // Number.isInteger alone is not enough: 1e21 passes it and renders as
  // "after 1e+21 turns". The bound is far above any real continuation
  // limit, so it only ever rejects a payload that was never a turn count.
  const claimed = payload.turns;
  const turns =
    typeof claimed === "number" && Number.isInteger(claimed) && claimed >= 0 && claimed <= TurnLimit
      ? claimed
      : null;
  // "0 turns" beside every declared stop would be noise, since the
  // count is of continuations rather than of work done.
  const count = turns && turns > 0 ? ` after ${turns} turn${turns === 1 ? "" : "s"}` : "";
  return why ? `the run stopped: ${why}${count}` : `the run stopped${count}`;
}

/**
 * `at` and `seq` order the list; `key` identifies an entry within it and
 * never changes, so the page can remember a decision about a particular
 * entry across a reconnect. `seq` shifts when a re-read returns more
 * messages; an event's key is its store position, stable across a break.
 *
 * @decision(DL-058)
 */
export interface MessageEntry {
  kind: "message";
  key: string;
  at: number;
  seq: number;
  role: string;
  text: string;
  /**
   * Set on a message this browser sent and the session has not said
   * back yet — the note to draw under it, never an empty string. A note
   * rather than a flag, so this file needs nothing from `steer.ts`.
   *
   * @decision(DL-059)
   */
  pending?: string;
  /**
   * A person's message's id, where it has one: what Cancel names.
   *
   * @decision(DL-019)
   */
  id?: string;
  /**
   * Where a person's message stands, from the stored inbox events. Unset
   * for everything else, and for a message older than the events.
   *
   * @decision(DL-019)
   */
  status?: MessageStatus;
  /** How it was sent and what follows it, from its enqueue. */
  delivery?: string;
  after?: string;
}

/**
 * Where a person's message stands. `unprocessed` is a queued message on
 * a session or job that has ended without reaching it.
 *
 * @decision(DL-019) @decision(DL-048)
 */
export type MessageStatus = "queued" | "processed" | "cancelled" | "unprocessed";

/** A message's status and when it last changed. */
export interface Inbox {
  status: MessageStatus;
  at: number;
  delivery?: string;
  after?: string;
}

const Enqueued = "session.inbox.enqueued";
const Delivered = "session.inbox.delivered";
const Cancelled = "session.inbox.cancelled";

/**
 * messageStatuses reads every person's message's status out of the log:
 * queued from its enqueue, processed from its delivery, cancelled from
 * its cancellation, and unprocessed when `ended` finds it still queued.
 * The sandbox's own frames only; a delivery or cancel with no enqueue
 * before it names nothing this page drew.
 *
 * @decision(DL-019) @decision(DL-048)
 */
export function messageStatuses(activity: Activity[], ended: boolean): Map<string, Inbox> {
  const out = new Map<string, Inbox>();
  for (const ev of activity) {
    if (ev.source !== "opencode" || !ev.message) continue;
    const at = millis(ev.at);
    switch (ev.name) {
      case Enqueued:
        if (ev.role === "user" && !out.has(ev.message)) {
          out.set(ev.message, { status: "queued", at, delivery: ev.delivery, after: ev.after });
        }
        break;
      case Delivered:
        if (out.has(ev.message)) out.set(ev.message, { ...out.get(ev.message)!, status: "processed", at });
        break;
      case Cancelled:
        if (out.get(ev.message)?.status === "queued") {
          out.set(ev.message, { ...out.get(ev.message)!, status: "cancelled", at });
        }
        break;
    }
  }
  if (ended) {
    for (const [id, inbox] of out) {
      if (inbox.status === "queued") out.set(id, { ...inbox, status: "unprocessed" });
    }
  }
  return out;
}

export interface EventEntry {
  kind: "event";
  key: string;
  at: number;
  seq: number;
  name: string;
  text: string;
  /** Whether spov wrote this rather than the sandbox. See isSeam. */
  seam: boolean;
  /** The call a tool event belongs to, from the stream's `call`. */
  call?: string;
  /** The tool's name, on the event of a call that carries it. */
  tool?: string;
  /** Set by `condense` on the one line that stands for a whole call. */
  calls?: ToolCall;
  /**
   * The ask this event opened, drawn as a card (DL-020). Set on a
   * permission.asked or form.created only.
   */
  ask?: Ask;
}

/**
 * One tool call, joined from the events OpenCode sends about it: the
 * name from `tool.input.started`, the input from `tool.input.ended` and
 * the ending from `tool.success` or `tool.failed`.
 *
 * @decision(DL-058)
 */
export interface ToolCall {
  name: string;
  input: string;
  state: "running" | "done" | "failed";
  error: string;
}

/**
 * One subagent's whole activity, drawn in the main transcript as a single
 * quiet row at the child's first entry. The child's own entries live in
 * `Timelines.children`.
 *
 * @decision(DL-111)
 */
export interface SubagentEntry {
  kind: "subagent";
  key: string;
  at: number;
  seq: number;
  /** The child conversation's OpenCode id. */
  id: string;
  /** The child's title, or "" when its conversation carries none. */
  title: string;
  /** How many tool calls the child made. */
  calls: number;
}

export type Entry = MessageEntry | EventEntry | SubagentEntry;

/** millis parses a timestamp to 0 rather than NaN, which would make the
 *  comparator return NaN and leave the sort order undefined. */
function millis(iso: string | undefined): number {
  const t = Date.parse(iso ?? "");
  return Number.isNaN(t) ? 0 : t;
}

/**
 * spokenBy reports whom an event is quoting, or "" when it is machinery.
 * Matched by name, not by "has text": a retry also carries text, but
 * that is machinery talking about the session.
 *
 * The event's own role wins; the name is only a fallback for an event
 * carrying none (an older server, or an unprojected frame).
 *
 * @decision(DL-058) @decision(DL-045)
 */
function spokenBy(name: string, role?: string): string {
  // The name decides whether this is speech at all; the role only
  // decides whose. Letting a role alone promote an event would widen
  // the predicate to any future frame carrying an item type and text.
  if (name.endsWith(".text.ended")) return (role ?? "").trim() || "assistant";
  if (name.endsWith(".inbox.enqueued")) return (role ?? "").trim() || "user";
  return "";
}

/**
 * buildTimeline merges the two sources into one ordered list.
 *
 * Entries with no timestamp sort first among themselves and keep arrival
 * order, rather than being scattered by a NaN comparison.
 */
/**
 * PendingSeq puts an echo after everything else that shares its
 * timestamp. An event's own seq is `seq + pos`, which is a store
 * position and unbounded, so the offset has to be well clear of one —
 * and well inside 2^53, or adding the index to it rounds every echo to
 * the same number and the sort stops distinguishing them.
 *
 * @decision(DL-059)
 */
const PendingSeq = 1e15;

/**
 * WaitingSeq puts a queued message after the log and before the echoes.
 *
 * @decision(DL-019)
 */
const WaitingSeq = PendingSeq / 2;

export interface Pending {
  /** The minted id; the entry's key comes from it and never changes. */
  key: string;
  /**
   * The message's id once the server answered, which the transcript's
   * copy will carry; the entry is keyed by it so the two are one element.
   *
   * @decision(DL-059)
   */
  id?: string;
  /** When the browser sent it. */
  at: number;
  text: string;
  /** What to draw under it. See MessageEntry.pending. */
  note: string;
}

/**
 * messageKey is a person's message's entry key, by its id, shared by the
 * echo and the transcript's copy so the page keeps one element as the
 * message arrives.
 *
 * @decision(DL-059)
 */
export function messageKey(id: string): string {
  return `msg:${id}`;
}

/**
 * The main list and each subagent's own, by the child's conversation id.
 *
 * @decision(DL-111)
 */
export interface Timelines {
  entries: Entry[];
  children: Map<string, Entry[]>;
}

/**
 * lay puts one conversation set's messages and events on a list, unsorted.
 * Roots and a child go through it alike; `child` changes who is speaking:
 * a `user` message is the parent agent's task prompt, a quiet row and
 * never a person's bubble, and an inbox frame is never promoted at all.
 *
 * @decision(DL-111) @decision(DL-058)
 */
function lay(conversations: Conversation[], activity: Activity[], child: boolean): { out: Entry[]; seq: number } {
  const out: Entry[] = [];
  // What has already been said, so a promoted event does not repeat
  // words the conversation already carries, or that another promoted
  // event carried first. Only promotion consults this: an event that is
  // never promoted stays a box even when it quotes a message verbatim.
  const said = new Set<string>();
  // The same question asked by identity rather than by wording, for a
  // user frame only, where ids exist to compare. `identified` keeps the
  // text fallback available for a transcript with no ids at
  // all; it is set only by a person's own message, since an assistant
  // id in an otherwise id-less transcript would disable the fallback
  // for the user messages that still need it.
  //
  // It rests on the inbox and the message store naming one message the
  // same way: `storedMessage` reads `inboxID` off the frame and
  // `parseMessages` reads `id` off /message, and two id spaces would
  // render every human message twice. Measured by S-LS1's
  // `steerMessageIDFor`; the text rule does not depend on it.
  //
  // @decision(DL-059) @decision(DL-019)
  const ids = new Set<string>();
  let identified = false;
  const remember = (role: string, id: string, body: string) => {
    if (id && role === "user") {
      ids.add(id);
      identified = true;
    }
    said.add(body);
  };
  let seq = 0;
  conversations.forEach((conv, c) => {
    (conv.messages ?? []).forEach((m, i) => {
      const text = m.text ?? "";
      // A message with nothing to show is dropped, not drawn empty: a
      // turn that spent itself on tool calls has no text at all, and a
      // tool's output is already on its own event row.
      //
      // @decision(DL-058)
      if (!text.trim()) return;
      // A child's `user` message is its task prompt, written by the
      // parent agent.
      //
      // @decision(DL-111)
      const role = child && m.role === "user" ? "prompt" : (m.role ?? "");
      remember(role, m.id ?? "", text.trim());
      const entry: MessageEntry = {
        kind: "message",
        key: `m${c}.${i}`,
        at: millis(m.at),
        seq: seq++,
        role,
        text,
      };
      if (m.id && role === "user") {
        entry.id = m.id;
        entry.key = messageKey(m.id);
      }
      out.push(entry);
    });
  });

  for (const ev of activity) {
    // See AuditMarkers: not a message, not an event box, not a seam.
    //
    // @decision(DL-059)
    if (isAudit(ev)) continue;
    // @decision(DL-058)
    if (isRecord(ev)) continue;
    // An ask is one card where it was asked; the frames that settle it
    // are read into its status (askStates), not drawn.
    //
    // @decision(DL-020)
    if (isAskFrame(ev)) {
      if (AskOpened.has(ev.name)) {
        out.push({
          kind: "event",
          key: `e${ev.pos}`,
          at: millis(ev.at),
          seq: seq + ev.pos,
          name: ev.name,
          text: "",
          seam: false,
          ask: ev.ask!,
        });
      }
      continue;
    }
    const name = ev.name ?? "";
    const text = ev.text ?? "";
    const at = millis(ev.at);
    // `seq + pos` continues past the messages read before this event, so
    // an undated event still lands after an undated message, and the
    // log's own order is kept among events.
    const pos = ev.pos;
    // A seam is never promoted to speech: it is spov talking about the
    // recording, not anyone talking in the session.
    const seam = isSeam(ev);
    const spoke = seam ? "" : spokenBy(name, ev.role);
    // A child's inbox frame is the parent agent's prompt, never a
    // person's message: it repeats the prompt row or stays an event.
    //
    // @decision(DL-111)
    if (child && spoke === "user") {
      if (said.has(text.trim())) continue;
    } else if (spoke && text.trim()) {
      // `.trim()`, not raw truthiness: "\n" is truthy and would promote a
      // bare newline to a speaker block with nothing readable in it.
      //
      // @decision(DL-058)
      const body = text.trim();
      // The id decides for a person's own words, and only where there
      // are ids to decide with. Everything else keeps the text rule.
      const id = spoke === "user" ? (ev.message ?? "") : "";
      const already = id ? ids.has(id) || (!identified && said.has(body)) : said.has(body);
      if (already) continue;
      remember(spoke, id, body);
      const entry: MessageEntry = { kind: "message", key: id ? messageKey(id) : `e${pos}`, at, seq: seq + pos, role: spoke, text };
      if (id) entry.id = id;
      out.push(entry);
      continue;
    }
    const entry: EventEntry = { kind: "event", key: `e${pos}`, at, seq: seq + pos, name, text, seam };
    if (!seam && ev.call) entry.call = ev.call;
    if (!seam && ev.tool) entry.tool = ev.tool;
    out.push(entry);
  }
  return { out, seq };
}

function byTime(a: Entry, b: Entry): number {
  return a.at - b.at || a.seq - b.seq;
}

/**
 * buildTimelines partitions its input by conversation before any speaker
 * rule: the roots are drawn as one list, each child as a list of its own
 * built by the same code. Only the roots' frames feed the reader's rules
 * (a person's message, its status, the echoes), so a subagent never
 * speaks as the reader. A frame naming no listed child is a root's.
 *
 * A child is one row in the main list, at its first entry, until the
 * card replaces it.
 *
 * @decision(DL-111) @decision(DL-059) @decision(DL-019)
 */
export function buildTimelines(
  conversations: Conversation[],
  activity: Activity[],
  pending: Pending[] = [],
  ended = false,
): Timelines {
  const kids = childIds(conversations);
  const rootActivity: Activity[] = [];
  const byChild = new Map<string, Activity[]>();
  for (const ev of activity) {
    // An ask stays where the reader can answer it, whoever asked.
    if (ev.session && kids.has(ev.session) && !isAskFrame(ev)) {
      const list = byChild.get(ev.session);
      if (list) list.push(ev);
      else byChild.set(ev.session, [ev]);
    } else {
      rootActivity.push(ev);
    }
  }

  const { out, seq } = lay(
    conversations.filter((c) => !c.parent),
    rootActivity,
    false,
  );

  const children = new Map<string, Entry[]>();
  for (const conv of conversations) {
    if (!conv.parent) continue;
    const events = byChild.get(conv.id) ?? [];
    const entries = lay([conv], events, true).out.sort(byTime);
    children.set(conv.id, entries);
    if (entries.length === 0) continue;
    const calls = condense(entries).filter((e) => e.kind === "event" && e.calls).length;
    const first = events.length > 0 ? Math.min(...events.map((e) => e.pos)) : 0;
    out.push({
      kind: "subagent",
      key: `sub:${conv.id}`,
      at: entries.reduce((n, e) => (n === 0 || (e.at !== 0 && e.at < n) ? e.at : n), 0),
      seq: seq + first,
      id: conv.id,
      title: (conv.title ?? "").trim(),
      calls,
    });
  }

  // A person's message is drawn where it was read: at its delivery once
  // processed, and below everything else while it still waits.
  //
  // @decision(DL-019)
  const statuses = messageStatuses(rootActivity, ended);
  const last = out.reduce((n, e) => (e.at > n ? e.at : n), 0);
  let waiting = 0;
  for (const e of out) {
    if (e.kind !== "message" || !e.id) continue;
    const inbox = statuses.get(e.id);
    if (!inbox) continue;
    e.status = inbox.status;
    if (inbox.delivery) e.delivery = inbox.delivery;
    if (inbox.after) e.after = inbox.after;
    if (inbox.status === "processed") e.at = inbox.at;
    if (inbox.status === "queued" || inbox.status === "unprocessed") {
      e.at = Math.max(e.at, last);
      e.seq = WaitingSeq + waiting++;
    }
  }

  // Echoes last, and their timestamp is floored to whatever is already
  // on screen: `at` is this browser's clock, not the sandbox's.
  //
  // @decision(DL-059)
  if (pending.length > 0) {
    pending.forEach((p, i) => {
      out.push({
        kind: "message",
        key: messageKey(p.id || p.key),
        at: Math.max(p.at, last),
        seq: PendingSeq + i,
        role: "user",
        text: p.text,
        pending: p.note,
      });
    });
  }

  return { entries: out.sort(byTime), children };
}

/** buildTimeline is the main list of `buildTimelines`. */
export function buildTimeline(
  conversations: Conversation[],
  activity: Activity[],
  pending: Pending[] = [],
  ended = false,
): Entry[] {
  return buildTimelines(conversations, activity, pending, ended).entries;
}

/**
 * A message on its own, a seam on its own, or a run of consecutive
 * structural events.
 */
export type Block =
  | { kind: "message"; key: string; entry: MessageEntry }
  | { kind: "seam"; key: string; entry: EventEntry }
  | { kind: "note"; key: string; entry: EventEntry }
  | { kind: "ask"; key: string; entry: EventEntry & { ask: Ask } }
  | { kind: "subagent"; key: string; entry: SubagentEntry }
  | { kind: "events"; key: string; events: EventEntry[] };

/**
 * CollapseFrom is the shortest run worth collapsing. The control sits
 * inside the run's first row, so two is already a saving.
 *
 * @decision(DL-058)
 */
export const CollapseFrom = 2;

/**
 * blocksOf groups consecutive structural events into runs. A message
 * always breaks a run, so a collapsed run can never hide what anyone
 * said, and a seam is never folded into one either — a gap must stay
 * visible. A run is identified by its first entry's key, so a run the
 * reader expanded stays expanded when the next event lands.
 *
 * @decision(DL-058)
 */
export function blocksOf(entries: Entry[]): Block[] {
  const out: Block[] = [];
  for (const entry of entries) {
    if (entry.kind === "message") {
      out.push({ kind: "message", key: entry.key, entry });
      continue;
    }
    // A subagent's row breaks a run as a message does.
    //
    // @decision(DL-111)
    if (entry.kind === "subagent") {
      out.push({ kind: "subagent", key: entry.key, entry });
      continue;
    }
    // An ask breaks a run as a message does: it must never be folded away.
    //
    // @decision(DL-020)
    if (entry.ask) {
      out.push({ kind: "ask", key: entry.key, entry: { ...entry, ask: entry.ask } });
      continue;
    }
    if (entry.seam) {
      // @decision(DL-058)
      const kind = NoteMarkers.has(entry.name) ? "note" : "seam";
      out.push({ kind, key: entry.key, entry });
      continue;
    }
    const last = out[out.length - 1];
    if (last && last.kind === "events") {
      last.events.push(entry);
      continue;
    }
    out.push({ kind: "events", key: entry.key, events: [entry] });
  }
  return out;
}

/**
 * shown is the part of a run that renders as lines: nothing until the
 * reader asks, since a folded run is drawn as its summary instead. A run
 * too short to collapse — one line — shows whole.
 *
 * @decision(DL-058)
 */
export function shown(events: EventEntry[], expanded: boolean): EventEntry[] {
  if (expanded || events.length < CollapseFrom) return events;
  return [];
}

/**
 * Plumbing is the OpenCode events that say a step, a text part, a
 * delivery or a turn began or ended. What they bracket is drawn already
 * — the words, the tool calls, the thoughts — so they are never drawn,
 * folded or open. A list of names to drop rather than to keep, so a
 * name this build has not heard of is still drawn.
 *
 * @decision(DL-058)
 */
export const Plumbing = new Set<string>([
  "session.step.started",
  "session.step.streamed",
  "session.step.ended",
  "session.reasoning.started",
  "session.text.started",
  "session.inbox.delivered",
  "session.inbox.cancelled",
  "session.inbox.delivery.changed",
  "session.instructions.updated",
  "session.execution.started",
  "session.execution.succeeded",
  "session.compaction.started",
  "session.viewed",
  // The model in use is the composer's to show (DL-014).
  "session.model.selected",
  // A client's choice of agent, such as an attached web UI opening on
  // its default; it says nothing about the work.
  "session.agent.selected",
]);

/**
 * Speech events that reached the timeline as events because they said
 * nothing, or nothing new: `buildTimeline` promotes the ones with words.
 */
const Spoken = new Set<string>(["session.text.ended", "session.inbox.enqueued"]);

const ToolPrefix = "session.tool.";

/**
 * condense prepares the timeline for drawing: it drops plumbing, and
 * joins every event of one tool call into one line at the call's first
 * event, wherever its others landed. An OpenCode event only: spov's own
 * rows pass through untouched, and an event with no call stays as it is.
 * Returns new entries; the input is not changed.
 *
 * @decision(DL-058)
 */
export function condense(entries: Entry[]): Entry[] {
  const out: Entry[] = [];
  const calls = new Map<string, ToolCall>();
  for (const e of entries) {
    if (e.kind !== "event" || e.seam) {
      out.push(e);
      continue;
    }
    if (Plumbing.has(e.name)) continue;
    if (Spoken.has(e.name) && !e.text.trim()) continue;
    if (e.call && e.name.startsWith(ToolPrefix)) {
      let call = calls.get(e.call);
      if (!call) {
        call = { name: "", input: "", state: "running", error: "" };
        calls.set(e.call, call);
        out.push({ ...e, calls: call });
      }
      joinCall(call, e);
      continue;
    }
    out.push(e);
  }
  return out;
}

/** joinCall folds one event of a call into what is known about it. */
function joinCall(call: ToolCall, e: EventEntry): void {
  if (e.tool) call.name = e.tool;
  switch (e.name) {
    case "session.tool.input.ended":
      call.input = e.text;
      break;
    case "session.tool.success":
      call.state = "done";
      break;
    case "session.tool.failed":
      call.state = "failed";
      call.error = e.text;
      break;
  }
}

/**
 * InputKeys are the fields that say what a call was about, best first:
 * the command a shell ran, the file a read or write touched, a search's
 * pattern, a fetch's address. Measured across every recorded call.
 */
const InputKeys = [
  "command",
  "path",
  "filePath",
  "pattern",
  "url",
  "query",
  "repository",
  "description",
  "summary",
  "code",
  "patchText",
  "id",
  // The question tool's, found here when its input was cut short;
  // a whole one is read by questionsOf.
  "question",
];

/** InputLimit bounds the summary drawn for a call, in characters. */
export const InputLimit = 200;

/**
 * toolInput is one line that says what a call was about, from its input
 * text. The input is JSON when it is whole; a long one can arrive cut
 * short, so a field is also looked for in the raw text. Falls back to
 * the text itself, flattened and bounded.
 *
 * @decision(DL-058)
 */
export function toolInput(input: string): string {
  const text = input.trim();
  if (!text) return "";
  let picked = "";
  try {
    const parsed: unknown = JSON.parse(text);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const fields = parsed as Record<string, unknown>;
      for (const k of InputKeys) {
        const v = fields[k];
        if (typeof v === "string" && v.trim()) {
          picked = v;
          break;
        }
      }
      if (!picked) picked = questionsOf(fields.questions);
      if (!picked && Object.keys(fields).length === 0) return "";
    }
  } catch {
    picked = rawField(text);
  }
  return oneLine(picked || text);
}

/**
 * questionsOf is what the question tool asks, from its `questions` list:
 * each question's text, joined. Empty when the list holds none.
 *
 * @decision(DL-058)
 */
function questionsOf(list: unknown): string {
  if (!Array.isArray(list)) return "";
  const asked: string[] = [];
  for (const q of list) {
    const text = q && typeof q === "object" ? (q as Record<string, unknown>).question : undefined;
    if (typeof text === "string" && text.trim()) asked.push(text.trim());
  }
  return asked.join(" · ");
}

/** rawField finds a known field's string in JSON that does not parse. */
function rawField(text: string): string {
  for (const k of InputKeys) {
    const m = new RegExp(`"${k}"\\s*:\\s*"((?:[^"\\\\]|\\\\.)*)`).exec(text);
    if (!m) continue;
    try {
      return JSON.parse(`"${m[1]}"`) as string;
    } catch {
      return m[1];
    }
  }
  return "";
}

/** oneLine flattens whitespace and bounds the result. */
function oneLine(s: string): string {
  const flat = s.replace(/\s+/g, " ").trim();
  return flat.length <= InputLimit ? flat : `${flat.slice(0, InputLimit)}…`;
}

/** Thought is the event that carries what the model thought. */
export const Thought = "session.reasoning.ended";

/**
 * runSummary says what a folded run holds, in counts: "3 tool calls,
 * 1 failed, 2 thoughts". It stands in for the run until the reader
 * opens it.
 *
 * @decision(DL-058)
 */
export function runSummary(events: EventEntry[]): string {
  let tools = 0;
  let failed = 0;
  let thoughts = 0;
  let retries = 0;
  let others = 0;
  for (const e of events) {
    if (e.calls) {
      tools++;
      if (e.calls.state === "failed") failed++;
    } else if (e.name === Thought) thoughts++;
    else if (e.name === Retry) retries++;
    else others++;
  }
  const parts: string[] = [];
  if (tools) parts.push(count(tools, "tool call"));
  if (failed) parts.push(`${failed} failed`);
  if (thoughts) parts.push(count(thoughts, "thought"));
  // @decision(DL-058)
  if (retries) parts.push(count(retries, "model retry", "model retries"));
  if (others) parts.push(count(others, tools || thoughts || retries ? "other event" : "event"));
  return parts.join(", ");
}

/**
 * subagentText is the one line a subagent's row says: its label, when
 * its conversation carries a title, and how many tool calls it made.
 *
 * @decision(DL-111)
 */
export function subagentText(entry: { title: string; calls: number }): string {
  const calls = entry.calls ? count(entry.calls, "tool call") : "no tool calls";
  return entry.title ? `Subagent · ${entry.title} · ${calls}` : `Subagent · ${calls}`;
}

function count(n: number, word: string, plural = `${word}s`): string {
  return `${n} ${n === 1 ? word : plural}`;
}

/** Retry is OpenCode retrying a model call that failed. */
const Retry = "session.retry.scheduled";

/**
 * trimBlankLines drops blank lines at either end of a message, keeping
 * the indentation of its first line. Plain text is drawn as written,
 * and a trailing "\n\n" would otherwise draw as an empty band.
 *
 * @decision(DL-058)
 */
export function trimBlankLines(text: string): string {
  return text.replace(/^(?:[ \t]*\r?\n)+/, "").trimEnd();
}

/**
 * TimeGap is how long the transcript must have been quiet before a time
 * divider is drawn.
 *
 * @decision(DL-058)
 */
export const TimeGap = 10 * 60 * 1000;

function blockAt(b: Block): number {
  return b.kind === "events" ? (b.events[0]?.at ?? 0) : b.entry.at;
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/**
 * timeDividers is the time to draw before a block, by the block's key:
 * before the first dated block, and before any that follows the one
 * before it by more than TimeGap. A new day adds the date. An undated
 * block, or one after an undated block, gets none.
 *
 * @decision(DL-058)
 */
export function timeDividers(blocks: Block[], now: Date): Map<string, string> {
  const out = new Map<string, string>();
  let prev: number | null = null;
  for (const b of blocks) {
    const at = blockAt(b);
    if (!at) {
      prev = 0;
      continue;
    }
    if (prev === null || (prev > 0 && at - prev > TimeGap)) {
      const when = new Date(at);
      const time = when.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
      const other = prev ? new Date(prev) : now;
      out.set(
        b.key,
        sameDay(when, other) ? time : `${when.toLocaleDateString(undefined, { day: "numeric", month: "short" })}, ${time}`,
      );
    }
    prev = at;
  }
  return out;
}
