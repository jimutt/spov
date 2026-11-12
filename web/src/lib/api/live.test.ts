import { afterEach, describe, expect, test, vi } from 'vitest';
import { logStream, sessionsStream } from './live';
import { describeProblem } from '../problem';

/** serve replies to each successive fetch with the next response, and
 *  hangs once they run out — a stream that stays open, rather than one
 *  that ends. */
function serve(...responses: Array<Response | (() => Response)>) {
  const fetchMock = vi.fn(async (_url: string, init: RequestInit) => {
    const next = responses.shift();
    if (!next) {
      // Nothing more to give: block until the caller aborts, which is
      // what a live stream with no further events looks like.
      return new Promise<Response>((_, reject) => {
        init.signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
      });
    }
    return typeof next === 'function' ? next() : next;
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function body(chunks: string[], opts: { then?: 'close' | 'error' } = {}): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(c) {
      if (i < chunks.length) {
        c.enqueue(enc.encode(chunks[i++]));
        return;
      }
      if (opts.then === 'error') c.error(new Error('connection reset'));
      else c.close();
    },
  });
}

function ok(b: ReadableStream<Uint8Array>): Response {
  return new Response(b, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

function frame(name: string, data: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

const SESSIONS = (ids: string[]) => ({
  sessions: ids.map((id) => ({ id })),
  at: '2026-09-01T00:00:00Z',
});

/** take reads n values then aborts, so a stream with no end still ends
 *  the test. */
async function take<T>(
  make: (ctl: AbortController) => AsyncGenerator<T>,
  n: number,
): Promise<{ got: T[]; ctl: AbortController }> {
  const ctl = new AbortController();
  const got: T[] = [];
  try {
    for await (const v of make(ctl)) {
      got.push(v);
      if (got.length >= n) break;
    }
  } catch {
    // An aborted stream throws on the way out; the values are what the
    // test is reading.
  }
  ctl.abort();
  return { got, ctl };
}

/** serveAlways answers every open the same way, forever — a server that
 *  is up but useless, which is the case a finite list of responses
 *  cannot express. */
function serveAlways(make: () => Response) {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => make());
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the lifetime boundary is not a gap', () => {
  // @decision(DL-058) @decision(DL-018)
  test('an orderly close reopens without reporting a gap', async () => {
    serve(
      ok(body([frame('sessions', SESSIONS(['a'])), frame('end', { reason: 'lifetime' })])),
      ok(body([frame('sessions', SESSIONS(['a', 'b']))])),
    );
    const gaps = vi.fn();
    const { got } = await take((ctl) => sessionsStream({ signal: ctl.signal, onGap: gaps }), 2);

    expect(got).toHaveLength(2);
    expect(got[1].sessions).toHaveLength(2);
    expect(gaps).not.toHaveBeenCalled();
  });

  // A session that can no longer produce events ends its log stream at
  // once. Reopening that would be a request loop, not a live tail: the
  // server's answer to the reopen is another immediate end.
  //
  // @decision(DL-018)
  test('a stopped source ends the stream instead of reopening it', async () => {
    const fetchMock = serve(
      ok(
        body([
          frame('log', { name: 'session.step.started', at: '2026-09-02T10:00:00Z', pos: 1 }),
          frame('end', { reason: 'stopped' }),
        ]),
      ),
      ok(body([frame('log', { name: 'session.step.started', at: '2026-09-02T10:00:01Z', pos: 2 })])),
    );
    const ended = vi.fn();
    const ctl = new AbortController();
    const got = [];
    for await (const ev of logStream('s1', { signal: ctl.signal, onEnded: ended })) got.push(ev);

    expect(got).toHaveLength(1);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // The last rows may have been written just before the end, so the
    // consumer is told to read once more.
    expect(ended).toHaveBeenCalledTimes(1);
  });

  // The server replays a session's whole log from `after`, so a reopen
  // that forgot the cursor would deliver everything the page already
  // has — once per lifetime boundary, for as long as the tab is open.
  //
  // @decision(DL-018)
  test('a reopen resumes from the last position rather than replaying', async () => {
    const fetchMock = serve(
      ok(
        body([
          frame('log', { name: 'a', at: '2026-09-02T10:00:00Z', pos: 7 }),
          frame('log', { name: 'b', at: '2026-09-02T10:00:01Z', pos: 8 }),
          frame('end', { reason: 'lifetime' }),
        ]),
      ),
      ok(body([frame('log', { name: 'c', at: '2026-09-02T10:00:02Z', pos: 9 })])),
    );
    const { got } = await take((ctl) => logStream('s1', { signal: ctl.signal }), 3);

    expect(got.map((e) => e.name)).toEqual(['a', 'b', 'c']);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(String(fetchMock.mock.calls[0][0])).not.toContain('after=');
    expect(String(fetchMock.mock.calls[1][0])).toContain('after=8');
  });

  // A break is the other way a stream reopens, and it must resume too:
  // the server replays from `after`, so a reopen that forgot the cursor
  // would re-deliver the whole log the page already holds.
  //
  // @decision(DL-058)
  test('a reopen after a break resumes from the last position too', async () => {
    const fetchMock = serve(
      ok(body([frame('log', { name: 'a', at: '2026-09-02T10:00:00Z', pos: 4 })], { then: 'error' })),
      ok(body([frame('log', { name: 'b', at: '2026-09-02T10:00:01Z', pos: 5 })])),
    );
    const gaps = vi.fn();
    const { got } = await take((ctl) => logStream('s1', { signal: ctl.signal, onGap: gaps }), 2);

    expect(got.map((e) => e.name)).toEqual(['a', 'b']);
    expect(String(fetchMock.mock.calls[1][0])).toContain('after=4');
    // A break is still a break: messages have no cursor, so the page is
    // told to read the conversation again.
    expect(gaps).toHaveBeenCalledTimes(1);
  });

  // The list has no finished state, so an `end` it does not understand
  // must reopen rather than stop the page with nothing said.
  //
  // @decision(DL-018)
  test('a stream with no finished state reopens on any end', async () => {
    serve(
      ok(body([frame('sessions', SESSIONS(['a'])), frame('end', { reason: 'stopped' })])),
      ok(body([frame('sessions', SESSIONS(['a', 'b']))])),
    );
    const { got } = await take((ctl) => sessionsStream({ signal: ctl.signal }), 2);
    expect(got).toHaveLength(2);
  });

  test('a dropped connection does report a gap', async () => {
    serve(
      ok(body([frame('sessions', SESSIONS(['a']))], { then: 'error' })),
      ok(body([frame('sessions', SESSIONS(['a', 'b']))])),
    );
    const gaps = vi.fn();
    const { got } = await take((ctl) => sessionsStream({ signal: ctl.signal, onGap: gaps }), 2);

    expect(got).toHaveLength(2);
    expect(gaps).toHaveBeenCalledTimes(1);
  });

  test('a body that simply ends is a gap too, not an orderly close', async () => {
    // No `end` event: the connection went away, however tidily the
    // reader saw the body finish.
    serve(
      ok(body([frame('sessions', SESSIONS(['a']))])),
      ok(body([frame('sessions', SESSIONS(['a', 'b']))])),
    );
    const gaps = vi.fn();
    const { got } = await take((ctl) => sessionsStream({ signal: ctl.signal, onGap: gaps }), 2);

    expect(got).toHaveLength(2);
    expect(gaps).toHaveBeenCalledTimes(1);
  });
});

describe('refusals', () => {
  test('a 403 is final and is not retried', async () => {
    const fetchMock = serve(
      new Response(JSON.stringify({ title: 'Forbidden', status: 403 }), {
        status: 403,
        headers: { 'Content-Type': 'application/problem+json' },
      }),
    );
    const ctl = new AbortController();
    await expect(async () => {
      for await (const _ of sessionsStream({ signal: ctl.signal })) void _;
    }).rejects.toThrow(/Forbidden/);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  // A limit's refusal is final like any other, so a page over its
  // stream limit does not reopen in a loop; it keeps the server's
  // detail for the page to show. @decision(DL-133)
  test('a 429 is final, is not retried, and keeps the server detail', async () => {
    const detail = 'You have 20 live views open, the most this server allows. Try again in 10 seconds.';
    const fetchMock = serve(
      new Response(JSON.stringify({ title: 'Too Many Requests', status: 429, detail }), {
        status: 429,
        headers: { 'Content-Type': 'application/problem+json', 'Retry-After': '10' },
      }),
    );
    const ctl = new AbortController();
    let caught: unknown;
    try {
      for await (const _ of logStream('s1', { signal: ctl.signal })) void _;
    } catch (err) {
      caught = err;
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(describeProblem(caught)).toEqual({ title: 'Too Many Requests', detail, status: 429 });
  });

  test('an error event ends the stream rather than reopening it', async () => {
    const fetchMock = serve(
      ok(body([frame('error', { problem: { title: 'the sandbox went away', status: 502 } })])),
    );
    const ctl = new AbortController();
    await expect(async () => {
      for await (const _ of logStream('s1', { signal: ctl.signal })) void _;
    }).rejects.toThrow(/sandbox went away/);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe('addressing', () => {
  // encodeURIComponent does not encode a dot, so `..` survives it and
  // resolves a segment away: /api/v1/sessions/../log is /api/v1/log.
  // The guard compares the normalised pathname against what was built,
  // which catches it however the reference is shaped.
  test('a reference cannot escape the path it is substituted into', async () => {
    serve();
    const ctl = new AbortController();
    await expect(async () => {
      for await (const _ of logStream('..', { signal: ctl.signal })) void _;
    }).rejects.toThrow(/not a reference/);
  });

  test('the streams address the endpoints the server registers', async () => {
    const fetchMock = serve(ok(body([frame('sessions', SESSIONS(['a']))])));
    await take((ctl) => sessionsStream({ signal: ctl.signal }), 1);
    expect(String(fetchMock.mock.calls[0][0])).toContain('/api/v1/sessions/events');

    vi.unstubAllGlobals();
    const logFetch = serve(ok(body([frame('log', { name: 'step', at: '2026-09-01T00:00:00Z' })])));
    await take((ctl) => logStream('s 1', { signal: ctl.signal }), 1);
    expect(String(logFetch.mock.calls[0][0])).toContain('/api/v1/sessions/s%201/log');
  });
});

describe('unknown frames', () => {
  test('a kind this build does not know is skipped', async () => {
    serve(ok(body([frame('weather', { sunny: true }), frame('sessions', SESSIONS(['a']))])));
    const { got } = await take((ctl) => sessionsStream({ signal: ctl.signal }), 1);
    expect(got).toHaveLength(1);
    expect(got[0].sessions).toHaveLength(1);
  });
});

describe('a stream that is up but useless', () => {
  // The reconnect loop must back off even though the connection opened
  // cleanly: a graceful shutdown, a drained replica, or a proxy closing
  // the body all produce a clean 200 followed by a clean end.
  //
  // @decision(DL-058) @decision(DL-018)
  test('backs off instead of re-reading once a second', async () => {
    vi.useFakeTimers();
    const enc = new TextEncoder();
    const fetchMock = serveAlways(
      () =>
        new Response(
          new ReadableStream({
            start(c) {
              // Opens cleanly, says nothing, ends cleanly.
              void enc;
              c.close();
            },
          }),
          { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
        ),
    );

    const gaps = vi.fn();
    const ctl = new AbortController();
    void (async () => {
      try {
        for await (const _ of sessionsStream({ signal: ctl.signal, onGap: gaps })) void _;
      } catch {
        /* aborted */
      }
    })();

    // A full minute of a server in this state.
    await vi.advanceTimersByTimeAsync(60_000);
    ctl.abort();

    // With exponential backoff from 1s, a minute allows roughly
    // 1+2+4+8+16+32 — six retries, not sixty.
    expect(gaps.mock.calls.length).toBeLessThanOrEqual(8);
    expect(fetchMock.mock.calls.length).toBeLessThanOrEqual(9);
  });
});
