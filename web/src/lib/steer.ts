import { ApiError, type Problem } from "./api/client";
import type { components } from "./api/types";
import type { Activity, Conversation } from "./timeline";

/**
 * The composer's logic, kept out of the component so it can be tested:
 * everything here takes plain data and returns plain data.
 *
 * @decision(DL-059) @decision(DL-019) @decision(DL-052)
 */

export type SteerRequest = components["schemas"]["SteerRequest"];
export type SteeredMessage = components["schemas"]["SteeredMessage"];
export type Delivery = NonNullable<SteerRequest["delivery"]>;
export type After = NonNullable<SteerRequest["after"]>;
export type JobState = components["schemas"]["JobView"]["state"];

/**
 * MessageIDPrefix is the prefix the server requires of a client-minted id.
 * It is written again as `MessageIDPrefix` in internal/server, whose
 * `validMessageID` answers 422 for an id that lacks it.
 *
 * @decision(DL-059)
 */
export const MessageIDPrefix = "msg_";

/** The characters an id's random half is drawn from. */
const IDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";

/** How many of them. */
const IDEntropy = 14;

/**
 * fraction is a random number in [0, 1), from the platform's CSPRNG
 * where there is one and Math.random otherwise. The randomness guards
 * against two ids colliding, not against guessing.
 */
function fraction(): number {
  const c = globalThis.crypto;
  if (c && typeof c.getRandomValues === "function") {
    const buf = new Uint32Array(1);
    c.getRandomValues(buf);
    return buf[0] / 2 ** 32;
  }
  return Math.random();
}

/**
 * newMessageID mints the id a send carries: `msg_`, the milliseconds in
 * hex so later ids sort later, then IDEntropy random characters. `now` and
 * `rand` are arguments so a test can pin the whole string.
 *
 * @decision(DL-059)
 */
export function newMessageID(now: number = Date.now(), rand: () => number = fraction): string {
  const ms = Number.isFinite(now) ? Math.max(0, Math.floor(now)) : 0;
  // Never truncated on the left: dropping a high digit would reverse the
  // ordering. The server bounds the whole id at 128 bytes.
  const stamp = ms.toString(16).padStart(12, "0");
  let tail = "";
  for (let i = 0; i < IDEntropy; i++) {
    // Clamped so a stub returning exactly 1 cannot index past the end.
    const at = Math.min(IDAlphabet.length - 1, Math.max(0, Math.floor(rand() * IDAlphabet.length)));
    tail += IDAlphabet[at];
  }
  return MessageIDPrefix + stamp + tail;
}

/**
 * An echo: a message this browser sent, drawn in the transcript before
 * the session has said it back.
 *
 * `key` is what this page minted and never changes, so a row is not
 * renumbered mid-send. `id` is the server's answer once there is one, and
 * is what reconciliation matches on. They differ only when the sandbox
 * minted its own id.
 *
 * @decision(DL-059)
 */
export interface Echo {
  key: string;
  id: string;
  text: string;
  /** When it was sent, by this browser's clock, for the timeline. */
  at: number;
  /** True until the server has answered. */
  sending: boolean;
}

/**
 * draftProblem says what is stopping a send, or "" when nothing is. It
 * checks only that there is text; the byte limit (MaxMessageBytes) is the
 * server's and is not restated here.
 *
 * @decision(DL-059) @decision(DL-019)
 */
export function draftProblem(text: string): string {
  return text.trim() ? "" : "a message needs some text";
}

/**
 * sendsOnEnter reports whether this keystroke is a send: Enter sends,
 * Shift-Enter starts a new line, and the Enter that commits an IME
 * candidate (`isComposing`) is not a send.
 *
 * @decision(DL-059) @decision(DL-061)
 */
export function sendsOnEnter(e: {
  key: string;
  shiftKey?: boolean;
  isComposing?: boolean;
  keyCode?: number;
}): boolean {
  if (e.key !== "Enter") return false;
  if (e.shiftKey) return false;
  // 229 is the legacy signal for the same thing, still sent by some
  // browsers alongside `isComposing`.
  if (e.isComposing || e.keyCode === 229) return false;
  return true;
}

/**
 * composerShown reports whether there is anything to send to. It reads
 * both the stream's end and the session row, because the row can be stale
 * only towards `running` and the stream ends only on a quiet tick.
 *
 * @decision(DL-059) @decision(DL-017)
 */
export function composerShown(state: string | undefined, tailEnded: boolean): boolean {
  if (tailEnded) return false;
  // Undefined means the row has not arrived yet. Showing nothing then means
  // the composer appears a beat late rather than appearing and vanishing.
  return state === "running";
}

/** Whether a message can be sent, and what to say when it cannot. */
export interface Sendability {
  can: boolean;
  why: string;
}

/**
 * KNOWN_JOB_STATES are the job states `sendability` decides explicitly.
 * `sendability` mirrors `jobRefusesAMessage` in internal/server so the page
 * can give the reason before a reader types. It is advisory: the server
 * decides, and an unknown state can send.
 *
 * @decision(DL-059) @decision(DL-052)
 */
export const KNOWN_JOB_STATES = [
  "queued",
  "provisioning",
  "running",
  "delivering",
  "succeeded",
  "failed",
  "blocked",
  "steered",
  "cancelled",
] as const satisfies readonly JobState[];

/**
 * everyJobStateIsKnown fails `astro check` when the API document gains a
 * job state that KNOWN_JOB_STATES does not list, so a new state is not
 * silently sendable through the permissive default arm below. Exported so
 * it counts as read; an unused constant is a hint.
 *
 * @decision(DL-059) @decision(DL-052)
 */
export const everyJobStateIsKnown: Exclude<
  JobState,
  (typeof KNOWN_JOB_STATES)[number]
> extends never
  ? true
  : never = true;

export function sendability(state: string | undefined): Sendability {
  switch (state) {
    case undefined:
      // No job. The session is interactive and none of DL-052 applies.
      return { can: true, why: "" };
    case "blocked":
      return {
        can: false,
        why: "The agent is parked on a question and is not reading its inbox.",
      };
    case "queued":
      return { can: false, why: "This job has no sandbox yet." };
    case "provisioning":
      return { can: false, why: "This job's agent has not been started yet." };
    case "delivering":
      return { can: false, why: "This job's run is over and it is being judged." };
    case "succeeded":
    case "failed":
    case "cancelled":
      return { can: false, why: "This job has finished." };
    default:
      return { can: true, why: "" };
  }
}

/**
 * offersAfter reports whether the composer asks what follows a message:
 * on a running job only. A steered job is already the reader's, and an
 * interactive session has no loop to continue or hand over.
 *
 * @decision(DL-059) @decision(DL-052)
 */
export function offersAfter(state: string | undefined, known: boolean): boolean {
  return known && state === "running";
}

/**
 * deliveryNote says what Send does. It may say the message is read once
 * the agent's current step finishes, that a job carries on after a
 * `continue` message and that it pauses for the reader after a handover.
 * It never says a turn stops; it names Interrupt only when `stoppable`,
 * that is, when Interrupt is on the page.
 *
 * @decision(DL-059) @decision(DL-052) @decision(DL-053)
 */
export function deliveryNote(
  state: string | undefined,
  after: After,
  known: boolean,
  stoppable = false,
): string {
  return sentOnly(state, after, known) + (stoppable ? " " + InterruptSentence : "");
}

/**
 * What Interrupt does to a message just sent, worded to follow "once its
 * current step finishes".
 *
 * @decision(DL-059) @decision(DL-053)
 */
const InterruptSentence = "Interrupt stops that step, and the agent reads it at once.";

function sentOnly(state: string | undefined, after: After, known: boolean): string {
  const read = "The agent reads it once its current step finishes, without stopping it.";
  // Until the jobs read answers, the sentence must be true of a job and of
  // an interactive session alike.
  if (!known || state === undefined) return read;
  if (state === "steered") return read + " The job is yours, so it then waits for you.";
  return after === "handover"
    ? read + " Once it has read it, the job pauses for you until you resume it."
    : read + " The job then carries on by itself.";
}

/**
 * toSteerRequest builds the body POST /api/v1/sessions/{ref}/messages
 * takes. It names no conversation on purpose: the server resolves an
 * omitted one and refuses a session with several. Delivery and after
 * are always sent, though steer and continue are the server's defaults.
 *
 * @decision(DL-059) @decision(DL-019) @decision(DL-052)
 */
export function toSteerRequest(text: string, delivery: Delivery, id: string, after: After = "continue"): SteerRequest {
  return { text: text.trim(), id, delivery, after };
}

/** What `send` needs of the API client, so a test can supply it. */
export type PostMessage = (body: SteerRequest) => Promise<{
  response: { ok: boolean; status: number };
  data?: SteeredMessage;
  error?: unknown;
}>;

/**
 * The three moments of a send, as the caller's own list records them.
 * Callbacks rather than a return value, because `open` happens before the
 * request so the echo renders at once.
 */
export interface EchoTrack {
  /** A message drawn optimistically, before the server has seen it. */
  open: (echo: Echo) => void;
  /** The server took it, under this id. */
  sent: (key: string, id: string) => void;
  /** The server refused it; the row comes back out. */
  failed: (key: string) => void;
}

/**
 * Failed is a send that did not arrive. `replay` is the id to send again
 * after a 5xx or a dropped connection, where the sandbox may already have
 * the message and dedupes on the id. It is "" after a 4xx, which the server
 * decides before anything reaches the sandbox, so the retry is a new message.
 *
 * @decision(DL-059)
 */
export class Failed extends Error {
  /** The id to send again, or "" when a fresh one should be minted. */
  readonly replay: string;
  readonly cause: unknown;

  constructor(message: string, replay: string, cause: unknown) {
    super(message);
    this.name = "Failed";
    this.replay = replay;
    this.cause = cause;
  }
}

/**
 * send posts one message and reports the echo at each step.
 *
 * A failure throws `Failed` after calling `track.failed`, carrying the
 * cause for the caller to render and the id to replay if the send
 * should be tried again. `replay` argument in, `replay` property out.
 *
 * @decision(DL-059)
 */
export async function send(
  text: string,
  delivery: Delivery,
  post: PostMessage,
  track: EchoTrack,
  replay = "",
  mint: () => string = newMessageID,
  clock: () => number = Date.now,
  after: After = "continue",
): Promise<string> {
  const body = text.trim();
  const key = replay || mint();
  track.open({ key, id: key, text: body, at: clock(), sending: true });
  let r: Awaited<ReturnType<PostMessage>>;
  try {
    r = await post(toSteerRequest(body, delivery, key, after));
  } catch (err) {
    track.failed(key);
    // The request may or may not have arrived, so the same id goes
    // again and the sandbox decides whether it is a duplicate.
    throw new Failed(err instanceof Error ? err.message : String(err), key, err);
  }
  if (!r.response.ok) {
    track.failed(key);
    const err = new ApiError(r.response.status, r.error as Problem | undefined);
    throw new Failed(err.message, r.response.status >= 500 ? key : "", err);
  }
  // The server's id wins where it has one. The server already falls back
  // to the minted id, so an empty field means an answer this build cannot
  // read, and the minted id is what was sent.
  const id = r.data?.id?.trim() || key;
  track.sent(key, id);
  return id;
}

/**
 * delivered is every message id the page has seen come back, from the
 * live stream's `session.inbox.enqueued` frames and from the stored
 * conversation. Either one clears an echo.
 *
 * @decision(DL-059) @decision(DL-019)
 */
export function delivered(conversations: Conversation[], activity: Activity[]): Set<string> {
  const seen = new Set<string>();
  for (const conv of conversations) {
    for (const m of conv.messages ?? []) {
      if (m.id) seen.add(m.id);
    }
  }
  for (const ev of activity) {
    if (ev.message) seen.add(ev.message);
  }
  return seen;
}

/**
 * unseen is the echoes still worth drawing: the ones the session has not
 * said back. An accepted echo stays until it is seen, even if the recorder
 * never stores it.
 *
 * @decision(DL-059) @decision(DL-017)
 */
export function unseen(
  echoes: Echo[],
  conversations: Conversation[],
  activity: Activity[],
): Echo[] {
  // Usually there are no echoes, and this runs on every arriving frame.
  if (echoes.length === 0) return echoes;
  const seen = delivered(conversations, activity);
  return echoes.filter((e) => !seen.has(e.id) && !seen.has(e.key));
}

/**
 * echoNote is the one-line label under an echo. "Sent" means the sandbox
 * has taken the message but the transcript does not show it yet.
 *
 * @decision(DL-059)
 */
export function echoNote(echo: Echo): string {
  return echo.sending ? "Sending…" : "Sent, not yet in the transcript";
}

/**
 * TurnStarted is the frame that says a turn has begun. No constant for it
 * exists in internal/server; `TestTheTurnStartedFrameReachesTheStore` is
 * what keeps it reaching the store.
 *
 * @decision(DL-059) @decision(DL-018)
 */
export const TurnStarted = "session.execution.started";

/**
 * The three ways a turn ends. All three clear in-flight, or Stop would
 * stay on screen after an interrupt.
 *
 * @decision(DL-059) @decision(DL-054)
 */
export const TurnEnders: readonly string[] = [
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
];

/**
 * turnInFlight reports whether a turn is running, from the last execution
 * frame by store position rather than arrival order. It ignores which
 * conversation the frame belongs to. If the page never saw a turn's
 * `started` frame, Stop stays hidden for that turn.
 *
 * Known gap: whether a subagent's session emits its own execution frames
 * onto this log is unmeasured; if it does, its end hides Stop mid-turn.
 * See DL-059 before reading `ev.session` here.
 *
 * @decision(DL-059) @decision(DL-019) @decision(DL-017)
 */
export function turnInFlight(activity: Activity[]): boolean {
  let at = -1;
  let running = false;
  for (const ev of activity) {
    // `name` and `pos` are required on the generated LogEvent, so neither
    // is defended against.
    if (ev.name !== TurnStarted && !TurnEnders.includes(ev.name)) continue;
    if (ev.pos < at) continue;
    at = ev.pos;
    running = ev.name === TurnStarted;
  }
  return running;
}

/**
 * turnsEnded counts the turn-ending frames the page holds, one per store
 * position. It only grows as the log is read, so a rise says a turn
 * ended even when a later frame has already started another, which
 * `turnInFlight` alone cannot show.
 *
 * @decision(DL-059)
 */
export function turnsEnded(activity: Activity[]): number {
  const seen = new Set<number>();
  for (const ev of activity) {
    if (TurnEnders.includes(ev.name)) seen.add(ev.pos);
  }
  return seen.size;
}

/**
 * canStop mirrors `jobRefusesAnInterrupt` in internal/server, as
 * `sendability` mirrors `jobRefusesAMessage`. The two refuse the same
 * states today, which steer.test.ts pins, but they answer different
 * questions. Do not fold this into `sendability().can`.
 *
 * @decision(DL-059) @decision(DL-053) @decision(DL-052)
 */
export function canStop(state: string | undefined): boolean {
  switch (state) {
    case "blocked":
    case "queued":
    case "provisioning":
    case "delivering":
    case "succeeded":
    case "failed":
    case "cancelled":
      return false;
    default:
      // Including `undefined`, an interactive session with no job. An
      // unknown state can stop; the server refuses in its own words.
      return true;
  }
}

/**
 * stopOutcome is what to say once a stop has been accepted. Interrupt
 * resumes what the reader sent (DL-053), so `interrupted: false` does not
 * mean the session was idle: a waiting message may just have started a
 * turn, and the sentence gives both readings.
 *
 * @decision(DL-059) @decision(DL-053)
 */
export function stopOutcome(interrupted: boolean): string {
  if (interrupted) return "The turn was interrupted. Anything you sent is read now.";
  return (
    "Interrupt was accepted. Either there was no turn running," +
    " or a message that was waiting has just started one."
  );
}

/**
 * resumable reports whether this job can be handed back to its loop
 * through `POST /api/v1/jobs/{id}/resume`, which accepts these two states.
 *
 * @decision(DL-059) @decision(DL-054)
 */
export function resumable(state: string | undefined): boolean {
  return state === "steered" || state === "blocked";
}

/**
 * resumeNote says what resuming does. A `steered` job's loop is held and
 * resuming releases it; a `blocked` job was never paused, so resuming only
 * moves the row and does not answer the request its agent is parked on.
 * Answering it in the transcript resumes the job by itself (DL-054).
 *
 * @decision(DL-059) @decision(DL-054)
 */
export function resumeNote(state: string | undefined): string {
  if (state === "blocked") {
    return (
      "Its agent is waiting on a permission request. Answer it in the" +
      " transcript and the job carries on by itself. Resuming hands the" +
      " job back to its loop without answering."
    );
  }
  return "The job carries on towards its original goal, and spov continues it again.";
}

/** What `stop` needs of the API client, so a test can supply it. */
export type PostInterrupt = () => Promise<{
  response: { ok: boolean; status: number };
  data?: { interrupted?: boolean };
  error?: unknown;
}>;

/**
 * stop halts the turn a session is running, and reports whether one was
 * actually running. An unreadable body reads as `false`.
 *
 * It repeats `unwrap`'s two lines rather than calling it: `unwrap` takes a
 * real `Response`, while this seam takes `{ok, status}` so a test can
 * supply one. Replacing it with `unwrap` breaks its tests.
 *
 * @decision(DL-059) @decision(DL-053)
 */
export async function stop(post: PostInterrupt): Promise<boolean> {
  const r = await post();
  if (!r.response.ok) {
    throw new ApiError(r.response.status, r.error as Problem | undefined);
  }
  return r.data?.interrupted === true;
}

/** What `cancel` needs of the API client, so a test can supply it. */
export type DeleteMessage = (id: string) => Promise<{
  response: { ok: boolean; status: number };
  error?: unknown;
}>;

/**
 * cancel asks the server to drop a queued message. Success says only
 * that the request reached the sandbox; whether the message was still
 * waiting is the stored `session.inbox.cancelled` or `delivered`, which
 * `messageStatuses` reads.
 *
 * @decision(DL-019)
 */
export async function cancel(id: string, del: DeleteMessage): Promise<void> {
  const r = await del(id);
  if (!r.response.ok) {
    throw new ApiError(r.response.status, r.error as Problem | undefined);
  }
}

/**
 * RunOver is whether a job's run can still read a queued message, by
 * state. A `Record`, so a new job state fails `astro check` until it is
 * placed.
 *
 * @decision(DL-048) @decision(DL-019)
 */
const RunOver: Record<JobState, boolean> = {
  queued: false,
  provisioning: false,
  running: false,
  blocked: false,
  steered: false,
  delivering: true,
  succeeded: true,
  failed: true,
  cancelled: true,
};

/**
 * inboxClosed reports that a queued message can no longer be read: the
 * stream has ended, the session is not running, or its job's run is over.
 *
 * @decision(DL-048) @decision(DL-019)
 */
export function inboxClosed(sessionState: string | undefined, tailEnded: boolean, jobState?: JobState): boolean {
  if (tailEnded) return true;
  if (sessionState !== undefined && sessionState !== "running") return true;
  return jobState !== undefined && RunOver[jobState] === true;
}

/**
 * queuedNote is the line under a person's message still waiting to be
 * read, worded by how it was sent and what follows it.
 *
 * @decision(DL-019) @decision(DL-059)
 */
export function queuedNote(delivery?: string, after?: string): string {
  const when =
    delivery === "queue"
      ? "Queued: the agent reads it when its current turn ends."
      : "Waiting: the agent reads it once its current step finishes.";
  return after === "handover" ? when + " The job then pauses for you." : when;
}

/**
 * composerStatus is the one line above the box: what the agent is doing
 * and whose the job is, or null when there is nothing to say. An ask in
 * the transcript waiting on the reader comes first.
 *
 * @decision(DL-059) @decision(DL-020)
 */
export function composerStatus(
  state: string | undefined,
  inFlight: boolean,
  asking = false,
): { mark: "run" | "wait"; word: string } | null {
  if (asking) return { mark: "wait", word: "Waiting on your answer" };
  if (state === "blocked") return { mark: "wait", word: "Waiting on a question" };
  if (state === "steered") {
    return inFlight
      ? { mark: "run", word: "Working on a step · paused for you" }
      : { mark: "wait", word: "Paused for you" };
  }
  return inFlight ? { mark: "run", word: "Working on a step" } : null;
}

/**
 * SendShort is the line under Send; `deliveryNote` is what its (i) opens.
 *
 * @decision(DL-059)
 */
export const SendShort = "Read once the current step finishes; nothing is stopped.";

/**
 * controlsNote explains the status bar's buttons, for its (i): only the
 * ones drawn, each under the button's own name.
 *
 * @decision(DL-059) @decision(DL-053)
 */
export function controlsNote(
  state: string | undefined,
  stoppable: boolean,
  releasable: boolean,
): { control: string; does: string }[] {
  const out: { control: string; does: string }[] = [];
  if (stoppable) {
    out.push({ control: "Interrupt", does: "Stops the step running now, and anything you sent is read at once." });
  }
  if (releasable) out.push({ control: "Resume job", does: resumeNote(state) });
  return out;
}
