import { describe, expect, test } from 'vitest';
import { TurnEnders, TurnStarted } from './steer';
import {
  delegatedCalls,
  pairSubagents,
  stateMark,
  stateWord,
  subagentState,
  type DelegatedCall,
  type SubagentState,
} from './subagent';
import { callFrames, childEnd, childStart, frame } from './subagent.frames';

/**
 * Which child belongs to which call, and how a subagent is going. The
 * pairing is a pure function over positions, so the cases are plain data.
 *
 * @decision(DL-112)
 */

const call = (id: string, start: number, o: Partial<DelegatedCall> = {}): DelegatedCall => ({
  call: id,
  start,
  at: 0,
  state: 'running',
  error: '',
  child: '',
  description: '',
  agent: '',
  background: false,
  ...o,
});

describe('reading the calls', () => {
  test('a call is known by its start and joined to its input and result', () => {
    const log = [
      ...callFrames('ses_p', 'call_a', 10, { description: 'Review it', agent: 'general', end: 'success', child: 'ses_c' }),
      ...callFrames('ses_p', 'call_b', 20, { description: 'Audit', background: true }),
    ];
    const got = delegatedCalls(log);
    expect(got.map((c) => [c.call, c.start, c.end, c.state, c.child, c.description, c.agent, c.background])).toEqual([
      ['call_a', 10, 13, 'done', 'ses_c', 'Review it', 'general', false],
      ['call_b', 20, undefined, 'running', '', 'Audit', 'general', true],
    ]);
  });

  test('only a subagent tool is a call, whichever of its names the build uses', () => {
    const other = [
      frame('ses_p', 1, 'session.tool.input.started', { call: 'call_x', tool: 'glob' }),
      frame('ses_p', 2, 'session.tool.success', { call: 'call_x', child: 'ses_nope' }),
    ];
    expect(delegatedCalls(other)).toEqual([]);
    const task = [frame('ses_p', 1, 'session.tool.input.started', { call: 'call_t', tool: 'task' })];
    expect(delegatedCalls(task).map((c) => c.call)).toEqual(['call_t']);
  });

  test('a failed call carries its error, and a follow-up carries the child it names', () => {
    const log = [
      ...callFrames('ses_p', 'call_f', 1, { end: 'failed' }),
      ...callFrames('ses_p', 'call_g', 10, { sessionID: 'ses_c' }),
    ];
    const [failed, follow] = delegatedCalls(log);
    expect([failed.state, failed.error]).toEqual(['failed', 'denied']);
    expect(follow.child).toBe('ses_c');
  });

  test('input that is not JSON, or an array, is read as nothing', () => {
    for (const text of ['{"description":', '[1]', '', 'null']) {
      const log = [
        frame('ses_p', 1, 'session.tool.input.started', { call: 'call_a', tool: 'subagent' }),
        frame('ses_p', 2, 'session.tool.input.ended', { call: 'call_a', text }),
      ];
      expect(delegatedCalls(log)[0]).toMatchObject({ description: '', agent: '', child: '' });
    }
  });
});

describe('pairing calls with children', () => {
  const kid = (id: string, first: number) => ({ id, first });

  test('a result that names its child is the link', () => {
    const got = pairSubagents([call('a', 1, { end: 9, child: 'c1' })], [kid('c1', 5)]);
    expect([...got.byCall]).toEqual([['a', 'c1']]);
    expect(got.standalone).toEqual([]);
  });

  test('parallel calls take their children oldest to oldest, whatever the order they end in', () => {
    const calls = [call('a', 1, { end: 20 }), call('b', 2, { end: 18 })];
    const got = pairSubagents(calls, [kid('c2', 6), kid('c1', 4)]);
    expect([...got.byCall]).toEqual([
      ['a', 'c1'],
      ['b', 'c2'],
    ]);
  });

  test('a named child holds against the span: the name decides, the span only fills the rest', () => {
    // b names c1, which began inside a's span too: a must take c2.
    const calls = [call('a', 1, { end: 20 }), call('b', 2, { end: 18, child: 'c1' })];
    const got = pairSubagents(calls, [kid('c1', 4), kid('c2', 6)]);
    expect(got.byCall.get('b')).toBe('c1');
    expect(got.byCall.get('a')).toBe('c2');
  });

  test('a call still running pairs with a child begun since it started, and not one begun before', () => {
    const calls = [call('a', 10)];
    expect(pairSubagents(calls, [kid('old', 3), kid('new', 12)]).byCall.get('a')).toBe('new');
    expect(pairSubagents(calls, [kid('old', 3)]).byCall.has('a')).toBe(false);
  });

  test('a child begun after the call ended is not its own', () => {
    const got = pairSubagents([call('a', 1, { end: 5 })], [kid('late', 9)]);
    expect(got.byCall.size).toBe(0);
    expect(got.standalone).toEqual(['late']);
  });

  test('a background child that outlives its call is paired by the name, or stands alone', () => {
    const calls = [call('a', 1, { end: 3, child: 'bg', background: true })];
    expect(pairSubagents(calls, [kid('bg', 14)]).byCall.get('a')).toBe('bg');
    // Without the id, past the call's end: drawn standalone, never dropped.
    const lost = pairSubagents([call('a', 1, { end: 3, background: true })], [kid('bg', 14)]);
    expect(lost.byCall.size).toBe(0);
    expect(lost.standalone).toEqual(['bg']);
  });

  test('a child no call has anything to do with stands alone, oldest first', () => {
    const got = pairSubagents([], [kid('z', 9), kid('y', 3)]);
    expect(got.standalone).toEqual(['y', 'z']);
  });

  test('a second call naming the same child is a follow-up, not another card', () => {
    const calls = [call('first', 1, { end: 9, child: 'c1' }), call('again', 20, { end: 25, child: 'c1' })];
    const got = pairSubagents(calls, [kid('c1', 4)]);
    expect([...got.byCall]).toEqual([['first', 'c1']]);
    expect([...got.followUps]).toEqual(['again']);
    expect(got.standalone).toEqual([]);
  });

  test('a call naming a child the page has not read yet waits for it and takes no other', () => {
    const got = pairSubagents([call('a', 1, { end: 9, child: 'ses_unread' })], [kid('c1', 4)]);
    expect(got.byCall.size).toBe(0);
    expect(got.standalone).toEqual(['c1']);
  });

  test('a child with no frames yet pairs only with a call still running', () => {
    const waiting = [kid('c1', Infinity)];
    expect(pairSubagents([call('a', 1)], waiting).byCall.get('a')).toBe('c1');
    expect(pairSubagents([call('a', 1, { end: 5 })], waiting).byCall.size).toBe(0);
  });

  test('is the same for the same input in any order', () => {
    const calls = [call('a', 1, { end: 20 }), call('b', 2, { end: 18 }), call('c', 3)];
    const kids = [kid('c3', 8), kid('c1', 4), kid('c2', 6)];
    const want = pairSubagents(calls, kids);
    const got = pairSubagents([...calls].reverse(), [...kids].reverse());
    expect([...got.byCall].sort()).toEqual([...want.byCall].sort());
    expect(got.standalone).toEqual(want.standalone);
  });
});

describe("a subagent's state", () => {
  const started = frame('ses_c', 5, TurnStarted);
  const ending = (name: string, pos = 9) => frame('ses_c', pos, name);

  test("is read from the child's own last execution frame, by position", () => {
    const states: [string, SubagentState][] = [
      ['session.execution.succeeded', 'done'],
      ['session.execution.failed', 'failed'],
      ['session.execution.interrupted', 'stopped'],
    ];
    for (const [name, want] of states) {
      expect(subagentState([started, ending(name)], undefined, false)).toBe(want);
      // Arriving in another order changes nothing.
      expect(subagentState([ending(name), started], undefined, false)).toBe(want);
    }
    expect(subagentState([ending('session.execution.succeeded'), frame('ses_c', 12, TurnStarted)], undefined, false)).toBe('running');
  });

  test("a child's own frames outrank the call's", () => {
    const done = call('a', 1, { end: 3, state: 'done' });
    expect(subagentState([started], done, false)).toBe('running');
    const failed = call('a', 1, { end: 3, state: 'failed' });
    expect(subagentState([started, ending('session.execution.succeeded')], failed, false)).toBe('done');
  });

  test('without execution frames it falls back to the call', () => {
    expect(subagentState(undefined, call('a', 1), false)).toBe('starting');
    expect(subagentState(undefined, call('a', 1, { end: 3, state: 'done' }), false)).toBe('done');
    expect(subagentState(undefined, call('a', 1, { end: 3, state: 'failed' }), false)).toBe('failed');
    // A background call is done at once and says nothing of its child.
    expect(subagentState(undefined, call('a', 1, { end: 3, state: 'done', background: true }), false)).toBe('starting');
    expect(subagentState([frame('ses_c', 6, 'session.tool.called')], call('a', 1, { end: 3, state: 'done', background: true }), false)).toBe(
      'running',
    );
    expect(subagentState([frame('ses_c', 6, 'session.tool.called')], undefined, false)).toBe('running');
  });

  test('a session that has ended stops what was still going, and nothing else', () => {
    expect(subagentState([started], undefined, true)).toBe('stopped');
    expect(subagentState(undefined, call('a', 1), true)).toBe('stopped');
    expect(subagentState([started, ending('session.execution.succeeded')], undefined, true)).toBe('done');
  });

  test("the frames it reads are the page's turn frames", () => {
    // steer.ts names them for the composer; a rename must reach both.
    const log = [...childStart('ses_c', 1), ...childEnd('ses_c', 3, 'x')];
    expect(log.some((e) => e.name === TurnStarted)).toBe(true);
    expect(TurnEnders).toContain(log[log.length - 1].name);
    for (const name of TurnEnders) {
      expect(subagentState([started, ending(name)], undefined, false)).not.toBe('running');
    }
  });

  test('every state has a word and a mark', () => {
    const all: SubagentState[] = ['starting', 'running', 'done', 'failed', 'stopped'];
    expect(all.map(stateWord)).toEqual(['Delegating…', 'Running', 'Done', 'Failed', 'Stopped']);
    expect(all.map(stateMark)).toEqual(['wait', 'run', 'done', 'fail', 'stop']);
  });
});
