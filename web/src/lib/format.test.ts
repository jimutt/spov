import { describe, expect, it, test } from 'vitest';
import {
  envRows,
  routedCredentialLine,
  routedCredentialTag,
  CREDENTIAL_LINES,
  IN_SANDBOX_NOTE,
  PAT_GRAPHQL_NOTE,
  GITLAB_GRAPHQL_NOTE,
  PROXIED_NOTES,
  credentialLine,
  ALL_NOTES,
  FELL_BACK_NOTE,
  PROXY_HELD,
  PROXIED_ENDPOINTS_NOTE,
  WarnWithin,
  age,
  gateTone,
  jobRunning,
  wallClock,
  jobTone,
  sessionSettling,
  byLiveThenRecent,
  day,
  duration,
  lifetime,
  live,
  money,
  repoHeadline,
  repoNames,
  resources,
  tone,
  clockBar,
  gateMark,
  gateName,
  jobMark,
  kindName,
  listState,
  sessionMark,
  stateWord,
  when,
  clock,
  clip,
  credentialTag,
  jobWord,
  verdictSummary,
} from './format';
import type { Session } from './format';

const NOW = new Date('2026-09-01T14:00:00Z');

function session(over: Partial<Session> = {}): Session {
  return {
    id: 's1',
    name: 'refactor-proxy',
    state: 'running',
    owner_id: 'u1',
    repos: [],
    sandbox: {},
    created_at: '2026-09-01T12:00:00Z',
    updated_at: '2026-09-01T12:00:00Z',
    estimated_cost: 0,
    ...over,
  } as Session;
}

describe('duration', () => {
  test('reads as the reference writes it, with minutes padded so a column lines up', () => {
    expect(duration(0)).toBe('0h 00m');
    expect(duration(4 * 60)).toBe('0h 04m');
    expect(duration(3600 + 12 * 60)).toBe('1h 12m');
    expect(duration(-5)).toBe('0h 00m');
  });

  test('days appear only once there are any', () => {
    expect(duration(23 * 3600)).toBe('23h 00m');
    expect(duration(25 * 3600)).toBe('1d 1h');
  });
});

describe('age', () => {
  test('a running session ages towards now', () => {
    expect(age(session(), NOW)).toBe(2 * 3600);
  });

  test('a stopped session stops ageing, so the list shows how long it took', () => {
    const s = session({ state: 'stopped', stopped_at: '2026-09-01T12:30:00Z' });
    expect(age(s, NOW)).toBe(30 * 60);
    // And an hour later it still says half an hour.
    expect(age(s, new Date('2026-09-01T15:00:00Z'))).toBe(30 * 60);
  });

  test('an unparseable timestamp is zero rather than NaN on the page', () => {
    expect(age(session({ created_at: 'not a date' }), NOW)).toBe(0);
  });
});

describe('lifetime', () => {
  // The track and its caption are the only place the page raises its
  // voice, so they have to be right about what they are measuring.
  //
  // @decision(DL-055)
  test('a running session is placed against its configured timeout', () => {
    const s = session({ sandbox: { timeout_secs: 4 * 3600 } });
    const l = lifetime(s, NOW);
    expect(l.fraction).toBeCloseTo(0.5);
    expect(l.note).toBe('of 4h');
    expect(l.warn).toBe(false);
  });

  test('it warns, and only warns, close to the limit', () => {
    const cap = 2 * 3600 + 10 * 60; // ten minutes left at NOW
    const l = lifetime(session({ sandbox: { timeout_secs: cap } }), NOW);
    expect(l.warn).toBe(true);
    expect(l.note).toBe('Stops in 10m');

    const roomy = lifetime(
      session({ sandbox: { timeout_secs: 2 * 3600 + WarnWithin + 60 } }),
      NOW,
    );
    expect(roomy.warn).toBe(false);
  });

  test('a finished session says how it ended, and never warns', () => {
    const hit = lifetime(
      session({ state: 'stopped', stopped_at: '2026-09-01T13:00:00Z', sandbox: { timeout_secs: 3600 } }),
      NOW,
    );
    expect(hit.note).toBe('Hit its limit');
    expect(hit.warn).toBe(false);
    expect(hit.fraction).toBe(1);

    const early = lifetime(
      session({ state: 'failed', stopped_at: '2026-09-01T12:20:00Z', sandbox: { timeout_secs: 3600 } }),
      NOW,
    );
    expect(early.note).toBe('Ended early');
    expect(early.warn).toBe(false);
  });

  test('with no timeout configured there is no bound to draw', () => {
    const l = lifetime(session({ sandbox: {} }), NOW);
    expect(l.fraction).toBe(0);
    expect(l.note).toBe('No limit set');
    expect(l.warn).toBe(false);
  });

  test('the track never overflows its rail', () => {
    const l = lifetime(session({ sandbox: { timeout_secs: 60 } }), NOW);
    expect(l.fraction).toBe(1);
  });
});

describe('money', () => {
  test('two places, and never NaN in front of a reader', () => {
    expect(money(0)).toBe('$0.00');
    expect(money(0.41)).toBe('$0.41');
    expect(money(Number.NaN)).toBe('$0.00');
    expect(money(Number.POSITIVE_INFINITY)).toBe('$0.00');
  });
});

describe('tone', () => {
  test('every state has a colour, and an unknown one still renders', () => {
    expect(tone('running')).toBe('run');
    expect(tone('starting')).toBe('start');
    expect(tone('failed')).toBe('fail');
    expect(tone('stopped')).toBe('stop');
    expect(tone('hibernating')).toBe('stop');
  });
});

describe('live', () => {
  test('is what is still costing money', () => {
    expect(live(session({ state: 'running' }))).toBe(true);
    expect(live(session({ state: 'starting' }))).toBe(true);
    expect(live(session({ state: 'stopped' }))).toBe(false);
    expect(live(session({ state: 'failed' }))).toBe(false);
  });
});

describe('day', () => {
  test('names today and yesterday, and dates anything older', () => {
    const now = new Date(2026, 8, 1, 14, 0);
    expect(day(new Date(2026, 8, 1, 9, 0).toISOString(), now)).toBe('today');
    expect(day(new Date(2026, 7, 31, 23, 0).toISOString(), now)).toBe('yesterday');
    expect(day(new Date(2026, 7, 20, 9, 0).toISOString(), now)).not.toMatch(/today|yesterday/);
  });
});

describe('summaries', () => {
  test('resources reads as the entry line does', () => {
    expect(resources({ cpu: 2, memory_mib: 4096 })).toBe('2 cpu, 4096 MiB');
    expect(resources({})).toBe('');
    expect(resources(undefined)).toBe('');
  });

  test('repoHeadline leads with the first repository and counts the rest', () => {
    const repo = (owner: string, name: string) => ({ platform: 'github', owner, name });
    expect(repoHeadline(session({ repos: [repo('acme', 'widgets')] }))).toEqual({ owner: 'acme/', name: 'widgets', more: 0 });
    expect(repoHeadline(session({ repos: [repo('acme', 'widgets'), repo('acme', 'tokens'), repo('x', 'y')] }))).toEqual({
      owner: 'acme/',
      name: 'widgets',
      more: 2,
    });
    expect(repoHeadline(session({ repos: [] }))).toBeNull();
    expect(repoHeadline(session({ repos: null }))).toBeNull();
  });

  test('repoNames is owner/name', () => {
    expect(repoNames(session({ repos: [{ platform: 'github', owner: 'acme', name: 'widgets' }] }))).toEqual([
      'acme/widgets',
    ]);
    expect(repoNames(session({ repos: [] }))).toEqual([]);
  });
});

describe('byLiveThenRecent', () => {
  // The server returns `order by id`, and an id is a ULID — lexically
  // ordered by creation. Rendered as received, the session you just
  // started sits at the bottom under everything already finished.
  //
  // @decision(DL-060)
  const at = (iso: string, state: Session['state'] = 'stopped') =>
    session({ id: iso, created_at: iso, state });

  test('newest first', () => {
    const older = at('2026-09-01T10:00:00Z');
    const newer = at('2026-09-01T12:00:00Z');
    expect([older, newer].sort(byLiveThenRecent)).toEqual([newer, older]);
  });

  test('anything live outranks anything finished, however old', () => {
    const oldLive = at('2026-08-01T10:00:00Z', 'running');
    const newDead = at('2026-09-01T12:00:00Z', 'stopped');
    expect([newDead, oldLive].sort(byLiveThenRecent)).toEqual([oldLive, newDead]);
  });

  test('starting counts as live', () => {
    const starting = at('2026-08-01T10:00:00Z', 'starting');
    const stopped = at('2026-09-01T12:00:00Z', 'stopped');
    expect([stopped, starting].sort(byLiveThenRecent)[0]).toBe(starting);
  });

  test('live sessions are ordered among themselves by recency', () => {
    const a = at('2026-09-01T10:00:00Z', 'running');
    const b = at('2026-09-01T12:00:00Z', 'running');
    expect([a, b].sort(byLiveThenRecent)).toEqual([b, a]);
  });

  test('an unparseable date does not poison the sort', () => {
    // A NaN comparator result leaves the order undefined rather than
    // merely wrong, so every entry must compare as a number.
    const broken = session({ id: 'x', created_at: 'not a date', state: 'stopped' });
    const fine = at('2026-09-01T12:00:00Z');
    const sorted = [broken, fine].sort(byLiveThenRecent);
    expect(sorted).toHaveLength(2);
    expect(sorted[0]).toBe(fine);
  });
});

describe('a job on the page', () => {
  test('a suspension is not drawn as a failure', () => {
    // blocked and steered are waiting on a person, not broken. Red
    // there reports a problem with the run where there is a question
    // for the reader.
    expect(jobTone('blocked')).toBe('start');
    expect(jobTone('steered')).toBe('start');
    expect(jobTone('failed')).toBe('fail');
  });

  test('a succeeded job is not the same colour as a cancelled one', () => {
    // The outcome is what this page exists to show. Grey for both says
    // "finished" and leaves the reader to read the word.
    expect(jobTone('succeeded')).toBe('run');
    expect(jobTone('cancelled')).toBe('stop');
  });

  test('an unknown job state renders rather than throwing', () => {
    expect(jobTone('something-newer')).toBe('stop');
  });

  test('an unverified gate is not drawn as a failed one', () => {
    // DL-050: missing evidence is missing evidence. Colouring it red
    // says a gate with nothing to judge on judged against you.
    expect(gateTone('unverified')).toBe('stop');
    expect(gateTone('failed')).toBe('fail');
    expect(gateTone('passed')).toBe('run');
  });

  test('a suspended job is still going to change', () => {
    // The page stops asking once a job is final. A blocked job is not
    // final — a human releases it — so giving up there leaves the page
    // permanently stale on exactly the state that needs attention.
    for (const state of ['queued', 'provisioning', 'running', 'delivering', 'blocked', 'steered']) {
      expect(jobRunning({ state } as never), state).toBe(true);
    }
    for (const state of ['succeeded', 'failed', 'cancelled']) {
      expect(jobRunning({ state } as never), state).toBe(false);
    }
  });

  test('an unknown state keeps the page asking rather than freezing it', () => {
    expect(jobRunning({ state: 'something-newer' } as never)).toBe(true);
    expect(jobRunning(undefined)).toBe(false);
  });
});

/**
 * Whether a session page should ask for its row. Lives here rather than
 * in the component so it can be tested.
 *
 * @decision(DL-017)
 */
describe('whether a session page should keep asking', () => {
  /**
   * A resource's `when` gates the FIRST load as well as the polling, so
   * a gate answering only "starting" never loads at all — no state can
   * arrive until something loads, and the page would sit on "loading"
   * forever.
   */
  test('it asks before anything has arrived', () => {
    expect(sessionSettling(undefined)).toBe(true);
  });

  test('it asks while the launch is in flight', () => {
    expect(sessionSettling('starting')).toBe(true);
  });

  test('it stops once the session has settled', () => {
    for (const state of ['running', 'stopped', 'failed']) {
      expect(sessionSettling(state), state).toBe(false);
    }
  });

  /**
   * A request that FAILED leaves no state, which is indistinguishable
   * from "nothing has arrived yet" — so the caller passes the failure
   * explicitly, or a page whose session 404s polls for the life of the
   * tab.
   */
  test('it stops when the request failed', () => {
    expect(sessionSettling(undefined, true)).toBe(false);
    expect(sessionSettling('starting', true)).toBe(false);
  });

  test('an unknown state does not poll forever', () => {
    // Unlike a job's state (DL-057), a session's is a closed set the
    // server owns, so an unrecognised one is far likelier a newer
    // terminal state than a new in-flight one.
    expect(sessionSettling('some-new-state')).toBe(false);
  });
});

// @decision(DL-049)
describe('wallClock', () => {
  const started = '2026-09-26T10:00:00Z';
  const job = (state: string, extra: Record<string, unknown> = {}) =>
    ({ state, max_duration: '1h0m0s', clock_started_at: started, ...extra }) as never;

  test('while running or blocked it says when the limit runs out', () => {
    for (const state of ['running', 'blocked']) {
      expect(wallClock(job(state)), state).toEqual({
        length: '1h',
        endsAt: '2026-09-26T11:00:00.000Z',
        note: '',
      });
    }
  });

  test('otherwise it says why the limit is not counting', () => {
    expect(wallClock(job('steered'))?.note).toBe('Paused while you steer');
    expect(wallClock(job('queued', { clock_started_at: undefined }))?.note).toBe('Once it starts');
    expect(wallClock(job('delivering'))?.note).toBe('The run is over');
  });

  test('the length reads as a limit, not as elapsed time', () => {
    expect(wallClock(job('queued', { max_duration: '2m0s' }))?.length).toBe('2m');
    expect(wallClock(job('queued', { max_duration: '1h30m0s' }))?.length).toBe('1h 30m');
    expect(wallClock(job('queued', { max_duration: '3h0m0s' }))?.length).toBe('3h');
  });

  // @decision(DL-049)
  test('the time already used comes off the end and off what is left', () => {
    expect(wallClock(job('running', { clock_used: '20m0s' }))?.endsAt).toBe('2026-09-26T10:40:00.000Z');
    expect(wallClock(job('steered', { clock_used: '20m0s', clock_started_at: undefined }))?.note).toBe(
      'Paused while you steer, 40m left',
    );
  });

  test('nothing for a finished job, or one with no clock', () => {
    expect(wallClock(job('succeeded'))).toBeNull();
    expect(wallClock(job('running', { max_duration: undefined }))).toBeNull();
    expect(wallClock(job('running', { max_duration: 'garbage' }))).toBeNull();
    expect(wallClock(undefined)).toBeNull();
  });
});

describe("what a sandbox holds", () => {
  // @decision(DL-043)
  it("words each kind, and reads an unknown one as the broader claim", () => {
    expect(credentialLine("renewed").label).toBe("App token");
    expect(credentialLine("renewed").note).toContain("renewed");
    expect(credentialLine("stored").note).toContain("cannot check");
    expect(credentialLine("hologram")).toEqual(CREDENTIAL_LINES.stored);
    expect(credentialLine("toString")).toEqual(CREDENTIAL_LINES.stored);
  });

  // @decision(DL-034)
  it("says where the token is held, and what a PAT's GraphQL still reaches", () => {
    expect(credentialLine("stored", "proxy", "forgejo").note).toBe(PROXIED_NOTES.stored);
    expect(credentialLine("stored", "proxy", "github").note).toBe(PROXIED_NOTES.stored + PAT_GRAPHQL_NOTE);
    expect(credentialLine("renewed", "proxy", "github").note).toBe(PROXIED_NOTES.renewed);
    // @decision(DL-034)
    expect(credentialLine("stored", "proxy", "gitlab").note).toBe(PROXIED_NOTES.stored + GITLAB_GRAPHQL_NOTE);
    expect(credentialLine("stored", "proxy", "gitea").note).toBe(PROXIED_NOTES.stored);
    expect(credentialLine("hologram", "proxy").note).toBe(PROXIED_NOTES.stored);
    expect(credentialLine("stored", "sandbox").note).toBe(CREDENTIAL_LINES.stored.note + IN_SANDBOX_NOTE);
    expect(credentialLine("renewed", "elsewhere")).toEqual(CREDENTIAL_LINES.renewed);
  });

  // @decision(DL-043)
  it("says when a token reaches every repository, and when all was asked for and not given", () => {
    expect(credentialLine("stored", "proxy", "github", "all").note).toBe(
      PROXY_HELD + ALL_NOTES.stored + PROXIED_ENDPOINTS_NOTE,
    );
    expect(credentialLine("stored", "sandbox", "github", "all").note).toBe(ALL_NOTES.stored + IN_SANDBOX_NOTE);
    expect(credentialLine("renewed", "sandbox", "github", "all").note).toBe(ALL_NOTES.renewed + IN_SANDBOX_NOTE);
    expect(credentialLine("renewed", "proxy", "github", "repos", "all").note).toBe(
      PROXIED_NOTES.renewed + FELL_BACK_NOTE,
    );
    expect(credentialLine("stored", "proxy", "forgejo", "repos", "repos").note).toBe(PROXIED_NOTES.stored);
  });
});

// @decision(DL-056)
describe('the state marks', () => {
  test('each family has its own mark, and only good endings get the tick', () => {
    expect(sessionMark('running')).toBe('run');
    expect(sessionMark('starting')).toBe('wait');
    expect(sessionMark('failed')).toBe('fail');
    expect(sessionMark('stopped')).toBe('stop');
    expect(jobMark('succeeded')).toBe('done');
    expect(jobMark('delivering')).toBe('run');
    expect(jobMark('steered')).toBe('wait');
    expect(jobMark('blocked')).toBe('wait');
    expect(jobMark('failed')).toBe('fail');
    expect(jobMark('cancelled')).toBe('stop');
    expect(gateMark('passed')).toBe('done');
    expect(gateMark('failed')).toBe('fail');
    expect(gateMark('unverified')).toBe('stop');
  });

  test('an unknown state still renders, as stopped', () => {
    expect(sessionMark('hibernating')).toBe('stop');
    expect(jobMark('hibernating')).toBe('stop');
    expect(stateWord('hibernating')).toBe('Hibernating');
  });

  test('words are display names, and the API values do not change', () => {
    expect(stateWord('steered')).toBe('Steered');
    expect(kindName('pr')).toBe('Pull request');
    expect(kindName('report')).toBe('Report');
    expect(kindName('audit')).toBe('Audit');
    expect(gateName('check')).toBe('Check');
    expect(gateName('conduct')).toBe('Conduct');
    expect(gateName('constructor')).toBe('Constructor');
  });

  test('a start time reads as a day and a time', () => {
    // Today in most time zones, yesterday east of UTC+10.
    expect(when('2026-09-01T12:00:00Z', NOW)).toMatch(/^(Today|Yesterday) /);
    expect(when('2026-09-01T12:00:00Z', NOW).endsWith(clock('2026-09-01T12:00:00Z'))).toBe(true);
    expect(when('not a date', NOW)).toBe('');
  });
});

// @decision(DL-060)
describe('listState', () => {
  const job = (state: string) => ({ id: 'j1', kind: 'pr', state }) as Session['job'];

  test('an interactive session shows its own state', () => {
    expect(listState(session({ state: 'starting' }))).toEqual({ mark: 'wait', word: 'Starting', why: '' });
  });

  test("a job's session shows the job's state", () => {
    expect(listState(session({ job: job('delivering') }))).toEqual({ mark: 'run', word: 'Delivering', why: '' });
  });

  test('a job waiting on a person says so', () => {
    for (const state of ['steered', 'blocked']) {
      expect(listState(session({ job: job(state) })).why, state).toBe('Waiting for you');
    }
  });

  test('a job whose sandbox is gone says what became of the sandbox', () => {
    const done = listState(session({ state: 'stopped', job: job('succeeded') }));
    expect(done).toEqual({ mark: 'done', word: 'Succeeded', why: 'Sandbox stopped' });
    expect(listState(session({ state: 'failed', job: job('running') })).why).toBe('Sandbox failed');
  });
});

// @decision(DL-057) @decision(DL-049)
describe('clockBar', () => {
  const started = '2026-09-01T13:00:00Z';
  const job = (state: string, extra: Record<string, unknown> = {}) =>
    ({ state, max_duration: '2h0m0s', clock_started_at: started, ...extra }) as never;

  test("a running job's bar is its wall clock", () => {
    const bar = clockBar(session(), job('running'), NOW)!;
    expect(bar.fraction).toBeCloseTo(0.5);
    expect(bar.start).toBe(`Started ${clock(started)}`);
    expect(bar.endLabel).toBe('Job stops at');
    expect(bar.endValue).toBe(clock('2026-09-01T15:00:00Z'));
    expect(bar.now).toBe(clock(NOW.toISOString()));
    expect(bar.warn).toBe(false);
  });

  // @decision(DL-049)
  test('time used before a resume counts, and the end agrees with wallClock', () => {
    const j = job('running', { clock_used: '30m0s' });
    const bar = clockBar(session(), j, NOW)!;
    expect(bar.fraction).toBeCloseTo(0.75);
    expect(bar.start).toBe(`Resumed ${clock(started)}`);
    expect(bar.endValue).toBe(clock(wallClock(j)!.endsAt!));
  });

  test('it warns inside the last quarter hour', () => {
    expect(clockBar(session(), job('running', { clock_used: '1h0m0s' }), NOW)!.warn).toBe(true);
    expect(clockBar(session(), job('running', { clock_used: '40m0s' }), NOW)!.warn).toBe(false);
  });

  test('a steered job shows a paused clock with no now', () => {
    const bar = clockBar(session(), job('steered', { clock_used: '30m0s' }), NOW)!;
    expect(bar.fraction).toBeCloseTo(0.25);
    expect(bar.now).toBeUndefined();
    expect(bar.endValue).toBe('1h 30m');
  });

  test("an interactive session's bar is its sandbox timeout", () => {
    const bar = clockBar(session({ sandbox: { timeout_secs: 4 * 3600 } }), undefined, NOW)!;
    expect(bar.fraction).toBeCloseTo(0.5);
    expect(bar.endLabel).toBe('Sandbox stops at');
    expect(bar.endValue).toBe(clock('2026-09-01T16:00:00Z'));
  });

  test('a delivering job falls back to the sandbox', () => {
    const bar = clockBar(session({ sandbox: { timeout_secs: 4 * 3600 } }), job('delivering'), NOW)!;
    expect(bar.endLabel).toBe('Sandbox stops at');
  });

  test('nothing once the session has ended, or with no limit', () => {
    expect(clockBar(session({ state: 'stopped', sandbox: { timeout_secs: 3600 } }), undefined, NOW)).toBeNull();
    expect(clockBar(session(), undefined, NOW)).toBeNull();
  });
});

// @decision(DL-057)
// @decision(DL-042)
describe('credentialTag names whose credential it is', () => {
  it('says the tier the launch recorded', () => {
    expect(credentialTag('renewed', 'proxy', 'repos', 'org')).toBe("App token, the organisation's");
    expect(credentialTag('stored', 'proxy', 'repos', 'user')).toContain('your own');
    expect(credentialTag('renewed', 'proxy', 'repos')).toBe('App token');
  });
});

describe('credentialTag', () => {
  test('is the kind alone in the usual case', () => {
    expect(credentialTag('renewed', 'proxy', 'repos')).toBe('App token');
    expect(credentialTag('stored', 'proxy')).toBe('Stored token');
  });

  test('keeps what departs from the usual case in view', () => {
    expect(credentialTag('stored', 'sandbox', 'all')).toBe('Stored token, every repository, inside the sandbox');
  });

  test('reads an unknown kind as a stored token', () => {
    expect(credentialTag('magic')).toBe('Stored token');
  });
});

// @decision(DL-057)
describe('clip', () => {
  test('leaves a short text alone', () => {
    expect(clip('  short goal ', 240)).toEqual({ head: 'short goal', clipped: false });
  });

  test('cuts a long text at a word', () => {
    const r = clip('alpha beta gamma delta', 13);
    expect(r).toEqual({ head: 'alpha beta…', clipped: true });
  });

  test('cuts mid-word when no word boundary is near', () => {
    expect(clip('x'.repeat(30), 10)).toEqual({ head: `${'x'.repeat(10)}…`, clipped: true });
  });
});

// @decision(DL-057)
describe('verdictSummary', () => {
  const v = (...outcomes: string[]) => verdictSummary(outcomes.map((outcome) => ({ outcome })));

  test('every gate passed', () => {
    expect(v('passed', 'passed', 'passed')).toEqual({ mark: 'done', word: 'Passed', open: false });
  });

  test('a gate spov could not verify beside passes is still a pass, said so', () => {
    expect(v('unverified', 'passed', 'passed')).toEqual({ mark: 'done', word: 'Passed, 1 unverified', open: false });
  });

  test('a failure decides it and opens the verdict', () => {
    expect(v('failed', 'passed', 'failed')).toEqual({ mark: 'fail', word: '2 gates failed', open: true });
    expect(v('passed', 'partial', 'failed').word).toBe('1 gate failed');
  });

  test('a partial deliverable opens the verdict', () => {
    expect(v('passed', 'partial', 'passed')).toEqual({ mark: 'part', word: 'Partial', open: true });
  });

  test('nothing verified is neutral', () => {
    expect(v('unverified', 'unverified')).toEqual({ mark: 'stop', word: 'Unverified', open: false });
  });
});

// A partial success is succeeded, drawn and worded as done in part.
//
// @decision(DL-060) @decision(DL-050)
describe('a partial success', () => {
  test('is a gold tick and "Succeeded, partly"', () => {
    expect(jobMark('succeeded', true)).toBe('part');
    expect(jobWord('succeeded', true)).toBe('Succeeded, partly');
    expect(jobMark('succeeded')).toBe('done');
    expect(jobWord('succeeded')).toBe('Succeeded');
  });

  test('says nothing on a state that is not a success', () => {
    expect(jobMark('failed', true)).toBe('fail');
    expect(jobWord('failed', true)).toBe('Failed');
  });

  // @decision(DL-101)
  test('a held sync job says so beside its state, in the list too', () => {
    expect(jobWord('succeeded', false, true)).toBe('Succeeded, held');
    expect(jobWord('failed', false, true)).toBe('Failed, held');
    const s = {
      id: 's', state: 'running', created_at: '2026-10-17T17:00:00Z',
      job: { id: 'j', kind: 'sync', state: 'succeeded', held: true },
    } as unknown as Session;
    expect(listState(s)).toMatchObject({ word: 'Succeeded, held', why: 'Held for spov jobs apply' });
  });

  test("reaches the session list's state column", () => {
    const s = {
      id: 's', state: 'stopped', created_at: '2026-10-17T17:00:00Z',
      job: { id: 'j', kind: 'report', state: 'succeeded', partial: true },
    } as unknown as Session;
    expect(listState(s)).toMatchObject({ mark: 'part', word: 'Succeeded, partly' });
  });
});

// @decision(DL-077)
describe("routedCredentialLine", () => {
  it("writes the hosts, paths and how the secret is sent", () => {
    expect(routedCredentialLine({ hosts: ["npm.acme.com"], scheme: "bearer" })).toBe("npm.acme.com, as a bearer");
    expect(
      routedCredentialLine({ hosts: ["repo.acme.com", "cdn.acme.com"], paths: ["/libs/"], scheme: "basic", user: "ci" }),
    ).toBe("repo.acme.com, cdn.acme.com /libs/, Basic as ci");
    expect(routedCredentialLine({ hosts: ["jfrog.acme.com"], scheme: "header", header: "X-JFrog-Art-Api" })).toBe(
      "jfrog.acme.com, in X-JFrog-Art-Api",
    );
  });
  it("tags whose it is and its preset", () => {
    expect(routedCredentialTag("user")).toBe("routed, your own");
    expect(routedCredentialTag("org", "npm")).toBe("npm, routed, the organisation's");
    expect(routedCredentialTag()).toBe("routed");
  });
});

// @decision(DL-096)
describe('envRows', () => {
  it('lists a session\'s variables in name order, an empty value kept', () => {
    expect(envRows({ NODE_ENV: 'test', A: '' })).toEqual([
      { name: 'A', value: '' },
      { name: 'NODE_ENV', value: 'test' },
    ]);
  });
  it('lists nothing for a session with none', () => {
    expect(envRows(undefined)).toEqual([]);
    expect(envRows(null)).toEqual([]);
  });
});
