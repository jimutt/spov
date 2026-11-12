import type { paths } from "./types";
import { baseURL, redirectToLogin } from "./client";
import { StreamProblem, frames, openStream } from "./sse";
import { PageSize } from "../paging";

/**
 * SessionsEvent is one frame of the caller's session-state stream, and
 * LogEvent one frame of a session's activity stream. Both come from the
 * generated document, so a field the server renames stops compiling.
 *
 * @decision(DL-058) @decision(DL-018)
 */
type StateFrame = NonNullable<
  paths["/api/v1/sessions/events"]["get"]["responses"][200]["content"]["text/event-stream"]
>[number];

type LogFrame = NonNullable<
  paths["/api/v1/sessions/{ref}/log"]["get"]["responses"][200]["content"]["text/event-stream"]
>[number];

export type SessionsEvent = Extract<StateFrame, { event: "sessions" }>["data"];
export type LogEvent = Extract<LogFrame, { event: "log" }>["data"];

/**
 * Backoff after a stream *breaks*. A lifetime close reopens at once —
 * it is the expected end of every stream — so this only ever applies to
 * a server that is failing, where retrying tightly would turn one
 * unreachable replica into a request flood. A stream the server called
 * finished does not reopen at all.
 */
const FirstRetry = 1_000;
const MaxRetry = 30_000;

/**
 * A 429 on opening a stream is retried once, after its Retry-After
 * bounded to these, before it is shown like any other refusal.
 *
 * @decision(DL-133)
 */
const MinLimitWait = 1_000;
const MaxLimitWait = 60_000;

/**
 * StableAfter is how long a connection must last, in ms, before it
 * counts as healthy and the backoff is forgiven. Keyed on connection
 * lifetime rather than on open alone, or a server that accepts and
 * closes without sending anything would never back off. Well under the
 * server's five-minute lifetime, so a healthy stream always clears it.
 *
 * @decision(DL-058) @decision(DL-018)
 */
const StableAfter = 30_000;

export interface LiveOptions {
  signal: AbortSignal;
  /**
   * onGap is called before a reopen that followed a break, and is where
   * a consumer re-reads whatever it holds that has no cursor of its own
   * — the conversation, which a break can leave stale even though the
   * event stream itself resumes cleanly. Never called at a lifetime
   * boundary (the server's five-minute `end`).
   *
   * @decision(DL-058) @decision(DL-018)
   */
  onGap?: () => void;
  /** Called when the connection state changes, for a status line. */
  onConnected?: (connected: boolean) => void;
  /**
   * onEnded is called once when the server says the source is finished
   * and the stream will not be reopened. A consumer re-reads whatever
   * it holds — the last rows may have been written just before the end
   * — and stops describing itself as live.
   *
   * @decision(DL-018)
   */
  onEnded?: () => void;
  /**
   * after is the cursor to open from, for a stream reopened once its
   * source started again: a resumed session's log continues where the
   * page's left off.
   *
   * @decision(DL-090)
   */
  after?: number;
}

/**
 * Resume names the query parameter that carries a cursor and reads it
 * off a frame, so a reopen continues where the last one stopped rather
 * than replaying what the caller already has.
 *
 * @decision(DL-018)
 */
interface Resume<T> {
  param: string;
  of: (frame: T) => number | undefined;
}

/**
 * liveStream yields one endpoint's payload frames, reopening across the
 * server's lifetime boundary for as long as the caller keeps reading.
 * Unlike `launchEvents`, which replays a finished launch and reopens
 * exactly once, these have no end of their own: the caller stops them
 * by aborting. A refusal is still final, except that a 429 is asked
 * once more after its Retry-After, and a 401 goes through the app's one
 * login funnel.
 *
 * @decision(DL-058) @decision(DL-018)
 */
async function* liveStream<T>(
  url: string,
  want: string,
  opts: LiveOptions,
  resume?: Resume<T>,
): AsyncGenerator<T> {
  let backoff = FirstRetry;
  // The last cursor yielded, sent on the next open. Without it every
  // reopen — one every five minutes, by design — replays the stream
  // from the beginning, and the caller sees everything twice.
  let cursor: number | undefined = opts.after;
  // Whether events may have been lost since the caller last read. Set
  // when a stream ends any way but the server's own, and reported once
  // the stream is back up — not once per attempt, so a server that is
  // refusing connections costs one re-read rather than one per retry.
  let missed = false;
  // Whether this run of refusals has had its one retry: a 429 is
  // retried once, and an open that succeeds earns the next one.
  //
  // @decision(DL-133)
  let limitRetried = false;

  for (;;) {
    if (opts.signal.aborted) return;
    let orderly = false;
    const openedAt = Date.now();
    try {
      const body = await openStream(
        resume && cursor !== undefined ? `${url}?${resume.param}=${cursor}` : url,
        opts.signal,
      );
      opts.onConnected?.(true);
      limitRetried = false;
      if (missed) {
        missed = false;
        opts.onGap?.();
      }
      for await (const frame of frames(body)) {
        let data: unknown;
        try {
          data = JSON.parse(frame.data);
        } catch {
          continue;
        }
        if (typeof data !== "object" || data === null) continue;
        switch (frame.name) {
          case want:
            cursor = resume?.of(data as T) ?? cursor;
            yield data as T;
            break;
          case "end":
            // An orderly close reopens without telling the caller
            // anything was missed, unless the source is finished — and
            // only a stream with a finished state may end that way; an
            // unknown reason must reopen rather than stop silently.
            //
            // @decision(DL-018)
            if (resume && (data as { reason?: string }).reason === "stopped") {
              opts.onConnected?.(false);
              opts.onEnded?.();
              return;
            }
            orderly = true;
            break;
          case "error":
            throw new StreamProblem(
              (data as { problem?: { title?: string } }).problem?.title ??
                "the stream reported an error",
              { final: true },
            );
          default:
            // A kind this build does not know, skipped like the launch
            // stream's unknown names.
            break;
        }
        if (orderly) break;
      }
    } catch (err) {
      if (opts.signal.aborted) return;
      // A refusal is final however it arrived, so the caller is told the
      // tail is dead before the error reaches them.
      if (err instanceof StreamProblem && err.status === 401) {
        opts.onConnected?.(false);
        redirectToLogin();
        throw err;
      }
      // A limit says when to come back, so it is asked once more then;
      // nothing is missed meanwhile that the cursor does not resume.
      //
      // @decision(DL-133)
      if (err instanceof StreamProblem && err.status === 429 && !limitRetried) {
        limitRetried = true;
        opts.onConnected?.(false);
        await sleep(Math.min(Math.max(err.retryAfter ?? MinLimitWait, MinLimitWait), MaxLimitWait), opts.signal);
        continue;
      }
      if (err instanceof StreamProblem && err.final) {
        opts.onConnected?.(false);
        throw err;
      }
      missed = true;
      opts.onConnected?.(false);
      await sleep(backoff, opts.signal);
      backoff = nextBackoff(backoff, openedAt);
      continue;
    }
    if (orderly) continue;
    // The body ended without an `end` event: a dropped connection,
    // however cleanly the reader saw it finish.
    missed = true;
    opts.onConnected?.(false);
    await sleep(backoff, opts.signal);
    backoff = nextBackoff(backoff, openedAt);
  }
}

/**
 * nextBackoff doubles the delay unless the connection that just ended
 * had lasted long enough to count as healthy, in which case the next
 * failure starts from the bottom again.
 *
 * Keyed on how long the stream was up rather than on whether it opened:
 * opening is what a failing server does successfully.
 */
function nextBackoff(current: number, openedAt: number): number {
  if (Date.now() - openedAt >= StableAfter) return FirstRetry;
  return Math.min(current * 2, MaxRetry);
}

/** sleep resolves after ms, or as soon as the caller aborts. */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done, { once: true });
  });
}

/** apiURL builds a same-origin API URL, refusing a reference that would
 *  escape the path it was substituted into. */
export function apiURL(path: string, ...segments: string[]): string {
  const holes = path.split("{}").length - 1;
  if (holes !== segments.length) {
    throw new StreamProblem(`${path} takes ${holes} segments, given ${segments.length}`, {
      final: true,
    });
  }
  let want = path;
  for (const s of segments) want = want.replace("{}", encodeURIComponent(s));
  const url = new URL(want, baseURL());
  if (url.pathname !== want) {
    throw new StreamProblem(`${JSON.stringify(segments.join("/"))} is not a reference`, {
      final: true,
    });
  }
  return url.toString();
}

/**
 * sessionsStream is the caller's whole session list, resent whenever any
 * of it changes. One stream serves the list page.
 *
 * @decision(DL-060) @decision(DL-018)
 */
export function sessionsStream(opts: LiveOptions, limit = PageSize): AsyncGenerator<SessionsEvent> {
  // The newest `limit` and every live session (DL-012).
  //
  // @decision(DL-060)
  return liveStream<SessionsEvent>(`${apiURL("/api/v1/sessions/events")}?limit=${limit}`, "sessions", opts);
}

/**
 * logStream is one session's recorded activity: the whole log on first
 * open, then whatever is written after it. Each frame carries its
 * position, so the reopen at the server's five-minute bound resumes
 * instead of replaying.
 *
 * @decision(DL-018) @decision(DL-058)
 */
export function logStream(ref: string, opts: LiveOptions): AsyncGenerator<LogEvent> {
  return liveStream<LogEvent>(apiURL("/api/v1/sessions/{}/log", ref), "log", opts, {
    param: "after",
    of: (ev) => ev.pos,
  });
}
