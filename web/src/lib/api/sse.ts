import type { paths } from "./types";
import { baseURL } from "./client";
import type { Problem } from "./client";

/**
 * LaunchEvent is one frame of a session's launch stream. The spec
 * describes the stream as an array because JSON Schema has no stream
 * type; that array is an envelope, not a batch, so indexing it gives
 * the discriminated union a single frame carries.
 *
 * @decision(DL-005)
 */
export type LaunchEvent = NonNullable<
  paths["/api/v1/sessions/{ref}/events"]["get"]["responses"][200]["content"]["text/event-stream"]
>[number];

/**
 * MaxEventBytes bounds one accumulated event, mirroring the Go client's
 * limit. It is counted in UTF-16 code units rather than bytes, so for
 * non-ASCII payloads it is a bound of the same order rather than exactly
 * the Go one — which is all it needs to be: nothing the spov server
 * sends comes near it.
 */
export const MaxEventBytes = 1 << 20;

/**
 * StreamProblem is a stream that ended without a result. A `status` and
 * `problem` mean the server refused the request and said why, and that
 * is final — retrying a 401 or a 404 cannot help. Without them the
 * connection broke, which is what a reopen is for.
 *
 * @decision(DL-005)
 */
export class StreamProblem extends Error {
  readonly status: number | undefined;
  readonly problem: Problem | undefined;
  /** final reports whether reopening the stream could ever help. */
  readonly final: boolean;

  constructor(message: string, opts: { status?: number; problem?: Problem; final?: boolean } = {}) {
    super(message);
    this.name = "StreamProblem";
    this.status = opts.status;
    this.problem = opts.problem;
    // A status means the server refused and said why, which no reopen
    // can change. Anything else is a broken connection unless the caller
    // says otherwise.
    this.final = opts.final ?? opts.status !== undefined;
  }
}

/** Frames are separated by a blank line, in any of SSE's line endings. */
const FRAME = /\r\n\r\n|\n\n|\r\r/;
const LINE = /\r\n|\n|\r/;

/**
 * KIND is every event name the contract carries, and whether it ends the
 * launch. Keyed by the generated union, so a kind the server renames or
 * drops stops compiling here. A kind the server adds is simply absent
 * and skipped.
 *
 * @decision(DL-005)
 */
const KIND: Record<LaunchEvent["event"], { terminal: boolean }> = {
  step: { terminal: false },
  note: { terminal: false },
  ready: { terminal: true },
  error: { terminal: true },
};

export interface Frame {
  name: string;
  data: string;
}

/**
 * frames yields the parsed frames of an event-stream body.
 *
 * The reader is cancelled on the way out however the loop ends, so a
 * consumer that breaks early — or a component that unmounts — does not
 * leave the response body open.
 */
export async function* frames(body: ReadableStream<Uint8Array>): AsyncGenerator<Frame> {
  const reader = body.getReader();
  // A TextDecoder rather than a TextDecoderStream: `stream: true` keeps
  // a multi-byte character split across two chunks intact, and the DOM
  // types do not let a TextDecoderStream sit in a pipeThrough here.
  const decoder = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        buf += decoder.decode();
        break;
      }
      buf += decoder.decode(value, { stream: true });
      const parts = buf.split(FRAME);
      // The last piece is whatever has arrived of the next frame. It
      // stays in the buffer: a chunk boundary can fall inside a CRLF,
      // and holding the remainder is what keeps that from reading as a
      // frame of its own.
      buf = parts.pop() ?? "";
      // What is left is exactly one unterminated frame, so bounding it
      // bounds one event — where bounding the whole read buffer would
      // refuse a single read that carried many perfectly good frames.
      // Final, like the Go client: a flood re-downloaded is worse.
      if (buf.length > MaxEventBytes) {
        throw new StreamProblem(`an event exceeded ${MaxEventBytes} bytes`, { final: true });
      }
      for (const part of parts) {
        const frame = parse(part);
        if (frame) yield frame;
      }
    }
    const last = parse(buf);
    if (last) yield last;
  } finally {
    await reader.cancel().catch(() => {});
  }
}

/** parse reads one frame's `event:` name and joined `data:` lines. */
function parse(frame: string): Frame | undefined {
  let name = "";
  const data: string[] = [];
  for (const line of frame.split(LINE)) {
    if (line === "" || line.startsWith(":")) {
      // A comment, typically the keepalive a sparse stream carries.
      continue;
    }
    if (line.startsWith("event:")) {
      name = line.slice("event:".length).trim();
    } else if (line.startsWith("data:")) {
      data.push(line.slice("data:".length).replace(/^ /, ""));
    }
  }
  if (!name || data.length === 0) return undefined;
  return { name, data: data.join("\n") };
}

/** endpoint is the launch stream's URL for one session reference. */
function endpoint(ref: string): string {
  const want = `/api/v1/sessions/${encodeURIComponent(ref)}/events`;
  const url = new URL(want, baseURL());
  // encodeURIComponent leaves dot segments alone, so a ref of ".."
  // resolves two segments away and this credentialed request would go
  // to a path the caller never named. Comparing the normalised pathname
  // against what was built catches that however the ref is shaped.
  if (url.pathname !== want) {
    throw new StreamProblem(`${JSON.stringify(ref)} is not a session reference`, { final: true });
  }
  return url.toString();
}

async function open(ref: string, signal: AbortSignal): Promise<ReadableStream<Uint8Array>> {
  return openStream(endpoint(ref), signal);
}

/**
 * openStream starts an event stream at url, turning a refusal into a
 * final StreamProblem carrying the server's own problem body. Shared
 * with live.ts, which opens the DL-018 streams.
 *
 * @decision(DL-055) @decision(DL-005)
 */
export async function openStream(
  url: string,
  signal: AbortSignal,
): Promise<ReadableStream<Uint8Array>> {
  const res = await fetch(url, {
    headers: { Accept: "text/event-stream" },
    credentials: "same-origin",
    signal,
  });
  if (!res.ok) {
    let problem: Problem | undefined;
    try {
      problem = (await res.json()) as Problem;
    } catch {
      // A non-2xx without a problem body is still a refusal; the status
      // is what the caller routes on.
    }
    await res.body?.cancel().catch(() => {});
    throw new StreamProblem(problem?.title ?? `the server answered ${res.status}`, {
      status: res.status,
      problem,
    });
  }
  if (!res.body) throw new StreamProblem("the stream carried no body");
  return res.body;
}

/**
 * launchEvents yields a session's launch events until one is terminal.
 * Mirrors the Go client's contract: a broken connection is reopened
 * once, from the beginning, since the server replays the launch rather
 * than resuming it — `onRestart` exists so a consumer accumulating
 * notes can clear them. A second break throws. Unknown event names are
 * skipped.
 *
 * @decision(DL-005) @decision(DL-002)
 */
export async function* launchEvents(
  ref: string,
  opts: { signal: AbortSignal; onRestart?: () => void },
): AsyncGenerator<LaunchEvent> {
  let broke: StreamProblem | undefined;

  for (let attempt = 0; attempt < 2; attempt++) {
    if (attempt > 0) opts.onRestart?.();
    let saw = false;
    try {
      for await (const frame of frames(await open(ref, opts.signal))) {
        saw = true;
        let data: unknown;
        try {
          data = JSON.parse(frame.data);
        } catch {
          // A frame that is not JSON is skipped like an unknown name.
          // The terminal event is what decides the launch, and a
          // malformed one surfaces below as a stream with no result.
          continue;
        }
        if (!isKnown(frame.name)) continue;
        // JSON.parse happily returns null, a number or a string, none of
        // which is an event payload. Yielding one would typecheck as the
        // payload it is not, and the page would throw reading a field
        // off it — so it is skipped like an unknown name, and a stream
        // whose terminal event was malformed ends with no result below.
        if (typeof data !== "object" || data === null) continue;
        // The sole cast, at the boundary where bytes become types: the
        // server's contract says this name carries this payload, and
        // the generated union is that contract.
        yield { event: frame.name, data } as LaunchEvent;
        if (KIND[frame.name].terminal) return;
      }
      broke = new StreamProblem(
        saw
          ? "the event stream ended without a result"
          : "the event stream closed before sending anything",
      );
    } catch (err) {
      if (opts.signal.aborted) throw err;
      if (err instanceof StreamProblem && err.final) throw err;
      broke = err instanceof StreamProblem ? err : new StreamProblem(String(err));
    }
  }
  throw broke ?? new StreamProblem("the event stream ended unexpectedly");
}

/** isKnown narrows a name to one the generated union carries. */
function isKnown(name: string): name is LaunchEvent["event"] {
  return Object.hasOwn(KIND, name);
}
