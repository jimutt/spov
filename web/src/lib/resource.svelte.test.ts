import { afterEach, describe, expect, test, vi } from 'vitest';
import { flushSync } from 'svelte';
import { resource } from './resource.svelte';

/** hidden drives document.visibilityState, which is read-only. */
function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', {
    value: state,
    configurable: true,
  });
  document.dispatchEvent(new Event('visibilitychange'));
}

/** settled lets pending promises run before the assertions. */
const settled = () => new Promise((r) => setTimeout(r, 0));

afterEach(() => {
  setVisibility('visible');
  vi.useRealTimers();
});

describe('Resource', () => {
  test('loads once on mount and holds what arrived', async () => {
    const load = vi.fn(async () => 'first');
    const stop = $effect.root(() => {
      const r = resource(load);
      flushSync();
      void (async () => {
        await settled();
        expect(load).toHaveBeenCalledTimes(1);
        expect(r.data).toBe('first');
        expect(r.loaded).toBe(true);
        expect(r.loading).toBe(false);
      })();
    });
    await settled();
    stop();
  });

  test('a failure keeps the previous body, so a blip does not blank the page', async () => {
    let turn = 0;
    const load = vi.fn(async () => {
      turn += 1;
      if (turn === 1) return 'good';
      throw new Error('the database is unhappy');
    });

    let r!: ReturnType<typeof resource<string>>;
    const stop = $effect.root(() => {
      r = resource(load);
      flushSync();
    });
    await settled();
    expect(r.data).toBe('good');

    await r.refresh();
    expect(r.data).toBe('good');
    expect(r.error?.message).toBe('the database is unhappy');

    stop();
  });

  test('a visible tab catches up when it is looked at again', async () => {
    const load = vi.fn(async () => 'x');
    let r!: ReturnType<typeof resource<string>>;
    const stop = $effect.root(() => {
      r = resource(load);
      flushSync();
    });
    await settled();
    expect(load).toHaveBeenCalledTimes(1);

    setVisibility('visible');
    await settled();
    expect(load).toHaveBeenCalledTimes(2);
    expect(r.data).toBe('x');

    stop();
  });

  test('a hidden tab is not polled', async () => {
    // A background tab that keeps asking is a request whose answer
    // nobody can see. On the usage page it would be one a minute for as
    // long as the tab exists.
    //
    // @decision(DL-055)
    vi.useFakeTimers();
    const load = vi.fn(async () => 'x');
    const stop = $effect.root(() => {
      resource(load, { every: 1000 });
      flushSync();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(load).toHaveBeenCalledTimes(1);

    setVisibility('hidden');
    await vi.advanceTimersByTimeAsync(5000);
    expect(load).toHaveBeenCalledTimes(1);

    setVisibility('visible');
    await vi.advanceTimersByTimeAsync(0);
    // The visibility change itself catches the page up...
    expect(load).toHaveBeenCalledTimes(2);
    // ...and the timer resumes from there.
    await vi.advanceTimersByTimeAsync(1000);
    expect(load).toHaveBeenCalledTimes(3);

    stop();
  });

  test('a gate holds the load until it opens', async () => {
    // The landing page is the reason this exists: it renders a
    // signed-out state, and a resource that loaded before the page knew
    // whether anyone was signed in would 401 and be redirected away.
    //
    // @decision(DL-055)
    const load = vi.fn(async () => 'x');
    let open = $state(false);
    const stop = $effect.root(() => {
      resource(load, { when: () => open });
      flushSync();
    });
    await settled();
    expect(load).not.toHaveBeenCalled();

    open = true;
    flushSync();
    await settled();
    expect(load).toHaveBeenCalledTimes(1);

    stop();
  });

  test('going away aborts a request that is still in flight', async () => {
    // A slow load that never settles on its own, which is the only case
    // where there is anything left to abort.
    let signal: AbortSignal | undefined;
    const load = vi.fn(
      (s: AbortSignal) =>
        new Promise<string>((resolve) => {
          signal = s;
          s.addEventListener('abort', () => resolve('abandoned'), { once: true });
        }),
    );
    const stop = $effect.root(() => {
      resource(load);
      flushSync();
    });
    await settled();
    expect(load).toHaveBeenCalledTimes(1);
    expect(signal?.aborted).toBe(false);

    stop();
    expect(signal?.aborted).toBe(true);
  });

  test('going away stops the timer', async () => {
    vi.useFakeTimers();
    const load = vi.fn(async () => 'x');
    const stop = $effect.root(() => {
      resource(load, { every: 1000 });
      flushSync();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(load).toHaveBeenCalledTimes(1);

    stop();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(load).toHaveBeenCalledTimes(1);
  });
});

/**
 * The polling gate, and the trap in it: `when` is read synchronously
 * inside the resource's effect, so a gate reading a value REPLACED on
 * every answer — a freshly parsed response body — changes its
 * dependency every load, tearing the effect down and re-running it
 * immediately rather than waiting for the interval.
 *
 * If the bug is reintroduced, a load resolving in a microtask starves
 * the macrotask queue and crashes the vitest worker rather than
 * reaching the assertion — a crashed worker here is this test failing.
 *
 * @decision(DL-057) @decision(DL-055)
 */
describe('a polled resource whose gate reads its own data', () => {
  test('does not re-load on every answer', async () => {
    let loads = 0;
    // A new object every time, exactly as a parsed response body is.
    const load = vi.fn(async () => {
      loads += 1;
      return { live: true };
    });

    let r!: ReturnType<typeof resource<{ live: boolean }>>;
    const stop = $effect.root(() => {
      r = resource(load, {
        every: 10_000,
        // The fix: a derived BOOLEAN. It recomputes on every answer, but
        // it does not notify dependents when the value is unchanged, so
        // the effect survives an answer that changed nothing. Reading
        // `r.data` here directly is the bug.
        when: () => alive,
      });
      // eslint-disable-next-line prefer-const
      var alive = $derived(r.data?.live !== false);
      flushSync();
    });

    // Long enough for many round trips, and far short of one interval.
    for (let i = 0; i < 20; i++) await settled();
    flushSync();
    stop();

    // One mount load. Anything more means the effect re-ran, which is
    // the flood: in a browser that is one request per round trip, per
    // tab, for as long as the job is live.
    expect(loads).toBe(1);
  });
});
