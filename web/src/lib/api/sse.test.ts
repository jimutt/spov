import { afterEach, describe, expect, test, vi } from 'vitest';
import { MaxEventBytes, StreamProblem, launchEvents, retryAfterMs } from './sse';
import type { LaunchEvent } from './sse';

/** stream turns chunks into a body, so a test can choose where the
 *  chunk boundaries fall — which is where the parser's edge cases are. */
function stream(chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream({
    start(c) {
      for (const chunk of chunks) c.enqueue(enc.encode(chunk));
      c.close();
    },
  });
}

/** broken sends its chunks and then errors, as a dropped connection
 *  does. One chunk per pull, because erroring a stream discards
 *  whatever is still queued — the chunks have to be read first. */
function broken(chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(c) {
      if (i < chunks.length) {
        c.enqueue(enc.encode(chunks[i++]));
        return;
      }
      c.error(new Error('connection reset'));
    },
  });
}

/** serve replies to each successive fetch with the next response. */
function serve(...responses: Array<Response | (() => Response)>) {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => {
    const next = responses.shift();
    if (!next) throw new Error('the stream was opened more times than expected');
    return typeof next === 'function' ? next() : next;
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function ok(body: ReadableStream<Uint8Array>): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

function frame(name: string, data: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

async function collect(gen: AsyncGenerator<LaunchEvent>): Promise<LaunchEvent[]> {
  const out: LaunchEvent[] = [];
  for await (const ev of gen) out.push(ev);
  return out;
}

const READY = { session: { id: 's1' } };

afterEach(() => vi.unstubAllGlobals());

describe('launchEvents', () => {
  test('yields a narrowing union and stops on a terminal event', async () => {
    serve(ok(stream([
      frame('step', { step: 'sandbox', state: 'started' }),
      frame('ready', READY),
      // Anything after a terminal event is a finished launch being
      // re-streamed, and must not reach the consumer.
      frame('note', { message: 'too late' }),
    ])));

    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events.map((e) => e.event)).toEqual(['step', 'ready']);

    // The point of the generated union: `data` narrows on the name.
    const first = events[0];
    if (first.event !== 'step') throw new Error('expected a step');
    expect(first.data.step).toBe('sandbox');
  });

  test('an error event is terminal too', async () => {
    serve(ok(stream([frame('error', { problem: { title: 'Launch failed', status: 502 } })])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events).toHaveLength(1);
    const only = events[0];
    if (only.event !== 'error') throw new Error('expected an error');
    expect(only.data.problem?.status).toBe(502);
  });

  test('joins multi-line data, skips keepalives and unknown names', async () => {
    serve(ok(stream([
      ':keepalive\n\n',
      'event: note\ndata: {"message":\ndata:  "two lines"}\n\n',
      // A kind a newer server added: skipped, not fatal.
      'event: teleport\ndata: {"whither":"elsewhere"}\n\n',
      // No name at all, and a name with no data: neither is an event.
      'data: {"orphan":true}\n\n',
      'event: note\n\n',
      frame('ready', READY),
    ])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events.map((e) => e.event)).toEqual(['note', 'ready']);
    const note = events[0];
    if (note.event !== 'note') throw new Error('expected a note');
    expect(note.data.message).toBe('two lines');
  });

  test('a frame whose data is not JSON is skipped', async () => {
    serve(ok(stream(['event: step\ndata: not json\n\n', frame('ready', READY)])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events.map((e) => e.event)).toEqual(['ready']);
  });

  test('CRLF endings, and a chunk boundary inside one', async () => {
    // The CR of the frame-ending CRLF arrives in one chunk and its LF in
    // the next. Normalising the buffer eagerly would read that lone CR
    // as a line ending and split the frame in the wrong place.
    serve(ok(stream([
      'event: step\r\ndata: {"step":"repo","state":"done"}\r\n\r',
      '\nevent: ready\r\ndata: ' + JSON.stringify(READY) + '\r\n\r\n',
    ])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events.map((e) => e.event)).toEqual(['step', 'ready']);
  });

  test('a frame split across chunks, and a final frame with no trailing blank line', async () => {
    serve(ok(stream(['event: st', 'ep\ndata: {"step":"re', 'po"}\n\nevent: ready\ndata: ' + JSON.stringify(READY)])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(events.map((e) => e.event)).toEqual(['step', 'ready']);
  });

  test('a multi-byte character split across chunks survives', async () => {
    const enc = new TextEncoder();
    const bytes = enc.encode(frame('note', { message: 'göteborg' }) + frame('ready', READY));
    // Split inside the two bytes of "ö".
    const cut = bytes.indexOf(0xc3) + 1;
    serve(ok(new ReadableStream({
      start(c) {
        c.enqueue(bytes.slice(0, cut));
        c.enqueue(bytes.slice(cut));
        c.close();
      },
    })));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    const note = events[0];
    if (note.event !== 'note') throw new Error('expected a note');
    expect(note.data.message).toBe('göteborg');
  });
});

describe('the request', () => {
  test('carries the cookie, asks for a stream, and names the session', async () => {
    const fetchMock = serve(ok(stream([frame('ready', READY)])));
    await collect(launchEvents('a session', { signal: new AbortController().signal }));

    const [url, init] = fetchMock.mock.lastCall!;
    // Cookie auth is the whole of the browser's credential here too.
    expect(init.credentials).toBe('same-origin');
    expect(new Headers(init.headers).get('Accept')).toBe('text/event-stream');
    // Absolute, like client.ts: a relative URL has nothing to resolve
    // against outside a document.
    expect(new URL(url).origin).toBe(location.origin);
    expect(new URL(url).pathname).toBe('/api/v1/sessions/a%20session/events');
    expect(init.signal).toBeDefined();
  });

  test('a ref that walks out of the path is refused before the cookie is sent', async () => {
    // ".." survives encodeURIComponent untouched and resolves away two
    // segments, so /api/v1/sessions/../events is /api/v1/events — a
    // credentialed request to a path the caller never named.
    const fetchMock = serve(ok(stream([])));
    // Only a bare dot segment traverses: a ref containing a slash has it
    // percent-encoded, which the URL parser does not normalise away.
    for (const ref of ['..', '.']) {
      const err = (await collect(launchEvents(ref, { signal: new AbortController().signal }))
        .then(() => undefined, (e: unknown) => e)) as StreamProblem;
      expect(err).toBeInstanceOf(StreamProblem);
      expect(err.final).toBe(true);
      expect(err.message).toContain('not a session reference');
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  test('a slash in a ref is encoded, not traversed', async () => {
    const fetchMock = serve(ok(stream([frame('ready', READY)])));
    await collect(launchEvents('../..', { signal: new AbortController().signal }));
    const [url] = fetchMock.mock.lastCall!;
    // It stays one path segment, so the server answers 404 rather than
    // the request landing somewhere else.
    expect(new URL(url).pathname).toBe('/api/v1/sessions/..%2F../events');
  });
});

describe('the event kinds', () => {
  test('every name the union carries is handled, and only ready and error end it', async () => {
    // KIND is keyed by the generated union, so this list is the contract
    // and the switch below is exhaustive by construction.
    for (const [name, ends] of [['step', false], ['note', false], ['ready', true], ['error', true]] as const) {
      serve(
        ok(stream([frame(name, {}), frame('note', { message: 'after' }), frame('ready', READY)])),
        () => ok(stream([frame('ready', READY)])),
      );
      const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
      expect(events[0].event, name).toBe(name);
      expect(events.length === 1, `${name} terminal=${ends}`).toBe(ends);
      vi.unstubAllGlobals();
    }
  });

  test('a payload that is not an object is skipped rather than yielded as one', async () => {
    // JSON.parse returns null, 42 and "x" quite happily; each would
    // typecheck as the payload it is not and throw in the page.
    const fetchMock = serve(
      ok(stream(['event: ready\ndata: null\n\n', 'event: step\ndata: 42\n\n', 'event: note\ndata: "x"\n\n'])),
      () => ok(stream([frame('ready', READY)])),
    );
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    // A malformed terminal event does not end the stream, so the reopen
    // gets a second chance at it — and that one is well-formed.
    expect(events.map((e) => e.event)).toEqual(['ready']);
    expect(events[0].data).not.toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe('failures', () => {
  test('a refusal is final: the problem body surfaces and the stream is not reopened', async () => {
    const fetchMock = serve(new Response(
      JSON.stringify({ title: 'Unauthorized', status: 401, detail: 'your session expired' }),
      { status: 401, headers: { 'Content-Type': 'application/problem+json' } },
    ));
    const err = await collect(launchEvents('s1', { signal: new AbortController().signal }))
      .then(() => undefined, (e: unknown) => e);
    expect(err).toBeInstanceOf(StreamProblem);
    const problem = err as StreamProblem;
    expect(problem.final).toBe(true);
    expect(problem.status).toBe(401);
    expect(problem.problem?.detail).toBe('your session expired');
    // Reopening a 401 cannot help, and looping on it is the failure mode
    // that ruled out EventSource.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('a refusal with no problem body still carries its status', async () => {
    serve(new Response('<html>gateway</html>', { status: 502 }));
    const err = (await collect(launchEvents('s1', { signal: new AbortController().signal }))
      .then(() => undefined, (e: unknown) => e)) as StreamProblem;
    expect(err.status).toBe(502);
    expect(err.problem).toBeUndefined();
    expect(err.message).toContain('502');
  });

  test('a broken connection is reopened once, from the beginning', async () => {
    const onRestart = vi.fn();
    const fetchMock = serve(
      ok(broken([frame('step', { step: 'sandbox', state: 'started' })])),
      () => ok(stream([
        // The server replays: the same step again, then the outcome.
        frame('step', { step: 'sandbox', state: 'done' }),
        frame('ready', READY),
      ])),
    );
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal, onRestart }));
    expect(events.map((e) => e.event)).toEqual(['step', 'step', 'ready']);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // The consumer is told before anything from the replay arrives, so
    // it can clear what it accumulated.
    expect(onRestart).toHaveBeenCalledTimes(1);
  });

  test('a second break throws rather than reconnecting forever', async () => {
    const fetchMock = serve(ok(broken([])), () => ok(broken([])));
    const err = (await collect(launchEvents('s1', { signal: new AbortController().signal }))
      .then(() => undefined, (e: unknown) => e)) as StreamProblem;
    expect(err).toBeInstanceOf(StreamProblem);
    expect(err.final).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test('a stream that closes with no terminal event is a broken stream', async () => {
    const fetchMock = serve(
      ok(stream([frame('note', { message: 'working' })])),
      () => ok(stream([])),
    );
    const err = (await collect(launchEvents('s1', { signal: new AbortController().signal }))
      .then(() => undefined, (e: unknown) => e)) as StreamProblem;
    expect(err.message).toContain('closed before sending anything');
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test('an abort propagates and is never retried', async () => {
    const ctl = new AbortController();
    const fetchMock = vi.fn(async () => {
      ctl.abort();
      throw new DOMException('aborted', 'AbortError');
    });
    vi.stubGlobal('fetch', fetchMock);
    await expect(collect(launchEvents('s1', { signal: ctl.signal }))).rejects.toThrow('aborted');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('an event larger than the bound is refused, and not re-downloaded', async () => {
    // No frame boundary, so it all accumulates as one unterminated event.
    const fetchMock = serve(ok(stream([`event: note\ndata: {"message":"${'x'.repeat(MaxEventBytes)}`])));
    const err = (await collect(launchEvents('s1', { signal: new AbortController().signal }))
      .then(() => undefined, (e: unknown) => e)) as StreamProblem;
    expect(err).toBeInstanceOf(StreamProblem);
    expect(err.message).toContain(`${MaxEventBytes}`);
    // Reopening replays the same flood from the beginning.
    expect(err.final).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('many good frames in one read are not mistaken for one huge event', async () => {
    // The buffer holds far more than the bound; the retained tail does
    // not, and the tail is what a single event actually is.
    const many = Array.from({ length: 200 }, (_, i) => frame('note', { message: 'x'.repeat(6000) + i })).join('');
    serve(ok(stream([many + frame('ready', READY)])));
    const events = await collect(launchEvents('s1', { signal: new AbortController().signal }));
    expect(many.length).toBeGreaterThan(MaxEventBytes);
    expect(events).toHaveLength(201);
    expect(events[200].event).toBe('ready');
  });

  test('leaving the loop early closes the body', async () => {
    const body = ok(stream([frame('note', { message: 'one' }), frame('note', { message: 'two' })]));
    serve(body);
    for await (const _ of launchEvents('s1', { signal: new AbortController().signal })) break;
    // A consumer that unmounts mid-launch must not leave the response
    // open; cancelling the reader is what locks the body.
    expect(body.bodyUsed || body.body?.locked).toBeTruthy();
  });
});

// A refusal's Retry-After is read in seconds or as a date, so a stream
// can wait as long as the server asked. @decision(DL-133)
describe('retryAfterMs', () => {
  test('reads seconds and dates, and nothing else', () => {
    const now = Date.parse('2026-11-12T09:00:00Z');
    expect(retryAfterMs('10', now)).toBe(10_000);
    expect(retryAfterMs(' 0 ', now)).toBe(0);
    expect(retryAfterMs('Thu, 12 Nov 2026 09:01:00 GMT', now)).toBe(60_000);
    expect(retryAfterMs('Thu, 12 Nov 2026 08:00:00 GMT', now)).toBe(0);
    expect(retryAfterMs('soon', now)).toBeUndefined();
    expect(retryAfterMs('-5', now)).toBeUndefined();
    expect(retryAfterMs(null, now)).toBeUndefined();
  });
});
