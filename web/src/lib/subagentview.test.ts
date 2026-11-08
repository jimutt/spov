import { describe, expect, test } from 'vitest';
import { callFrames, childEnd, childStart, toolFrames } from './subagent.frames';
import { backgroundConversations, backgroundLog, subagentConversations, subagentLog } from './subagent.fixture';
import { buildTimelines, type Conversation } from './timeline';
import { agentOf, isPlainClick, scopeOf, siblingsOf, subagentTree, trailTo, viewHref } from './subagentview';

/**
 * The tree of subagents the page holds and where its links lead, over
 * the timelines `buildTimelines` draws: the measured fixtures for the
 * plain cases, built frames for siblings and a child under a child.
 *
 * @decision(DL-113)
 */

const P = 'ses_parent';
const convs: Conversation[] = [
  { id: P, messages: [] },
  { id: 'ses_c1', parent: P, title: 'First', messages: [] },
  { id: 'ses_c2', parent: P, title: 'Second', messages: [] },
  { id: 'ses_g1', parent: 'ses_c1', title: 'Helper', messages: [] },
  { id: 'ses_c3', parent: P, title: 'Third', messages: [] },
];
// Three children of the main conversation, the first with a child of its
// own, begun in the order of their calls.
const log = [
  ...callFrames(P, 'call_a', 10, { description: 'one', end: 'success', child: 'ses_c1' }),
  ...childStart('ses_c1', 20),
  ...callFrames('ses_c1', 'call_h', 30, { description: 'help', end: 'success', child: 'ses_g1' }),
  ...childStart('ses_g1', 40),
  ...toolFrames('ses_g1', 'call_t', 50, 'grep', { pattern: 'x' }),
  ...childEnd('ses_g1', 60, 'helped'),
  ...childEnd('ses_c1', 70, 'led'),
  ...callFrames(P, 'call_b', 80, { description: 'two', end: 'success', child: 'ses_c2' }),
  ...childStart('ses_c2', 90),
  ...toolFrames('ses_c2', 'call_u', 100, 'read', { path: 'a.go' }),
  ...toolFrames('ses_c2', 'call_v', 110, 'read', { path: 'b.go' }),
  ...callFrames(P, 'call_c', 120, { description: 'three' }),
  ...childStart('ses_c3', 130),
];

describe('the tree of subagents', () => {
  const tree = subagentTree(buildTimelines(convs, log));

  test('lists every subagent with its parent and depth, each followed by its own', () => {
    expect(tree.order.map((n) => [n.id, n.parent, n.depth])).toEqual([
      ['ses_c1', '', 0],
      ['ses_g1', 'ses_c1', 1],
      ['ses_c2', '', 0],
      ['ses_c3', '', 0],
    ]);
  });

  test('carries what the card says: its label, state and count of calls', () => {
    const got = (id: string) => tree.byId.get(id);
    expect(got('ses_c1')).toMatchObject({ label: 'First', state: 'done', calls: 0 });
    expect(got('ses_g1')).toMatchObject({ label: 'Helper', state: 'done', calls: 1 });
    expect(got('ses_c2')).toMatchObject({ label: 'Second', state: 'running', calls: 2 });
    expect(got('ses_c3')).toMatchObject({ label: 'Third', state: 'running' });
  });

  test('places a subagent among the children of its own parent only', () => {
    expect(siblingsOf(tree, 'ses_c1')).toMatchObject({ place: 1, total: 3, previous: undefined });
    expect(siblingsOf(tree, 'ses_c1')?.next?.id).toBe('ses_c2');
    expect(siblingsOf(tree, 'ses_c2')?.previous?.id).toBe('ses_c1');
    expect(siblingsOf(tree, 'ses_c2')?.next?.id).toBe('ses_c3');
    expect(siblingsOf(tree, 'ses_c3')).toMatchObject({ place: 3, total: 3, next: undefined });
    // The helper is alone under its parent: no Previous, no Next.
    expect(siblingsOf(tree, 'ses_g1')).toMatchObject({ place: 1, total: 1, previous: undefined, next: undefined });
    expect(siblingsOf(tree, 'ses_nope')).toBeUndefined();
  });

  test('the trail runs from the top down to the subagent itself', () => {
    expect(trailTo(tree, 'ses_g1').map((n) => n.id)).toEqual(['ses_c1', 'ses_g1']);
    expect(trailTo(tree, 'ses_c2').map((n) => n.id)).toEqual(['ses_c2']);
    expect(trailTo(tree, 'ses_nope')).toEqual([]);
  });

  test('a scope is the subagent and everything under it', () => {
    expect(scopeOf(tree, 'ses_c1')).toEqual(['ses_c1', 'ses_g1']);
    expect(scopeOf(tree, 'ses_c2')).toEqual(['ses_c2']);
    expect(scopeOf(tree, 'ses_nope')).toEqual([]);
  });

  test('a call with no child yet is not a subagent, and a session with none has an empty tree', () => {
    const delegating = subagentTree(buildTimelines([{ id: P, messages: [] }], callFrames(P, 'call_a', 10, { description: 'x' })));
    expect(delegating.order).toEqual([]);
    expect(subagentTree(buildTimelines([], [])).order).toEqual([]);
  });
});

describe('the tree over the measured fixtures', () => {
  test('a foreground child is the one subagent, named by its title', () => {
    const tree = subagentTree(buildTimelines(subagentConversations, subagentLog, [], false));
    expect(tree.order.map((n) => [n.label, n.state, n.depth])).toEqual([['Review the retry change', 'done', 0]]);
    expect(siblingsOf(tree, tree.order[0].id)).toMatchObject({ place: 1, total: 1 });
  });

  test('a background child no call named is in the tree too', () => {
    const tree = subagentTree(buildTimelines(backgroundConversations, backgroundLog, [], false));
    expect(tree.order).toHaveLength(1);
    expect(tree.order[0].parent).toBe('');
  });
});

describe('the address of a view', () => {
  test('reads the subagent a query string names, and none as the main conversation', () => {
    expect(agentOf('?id=ses_1&agent=ses_abc')).toBe('ses_abc');
    expect(agentOf('?agent=ses_abc&id=ses_1')).toBe('ses_abc');
    expect(agentOf('?id=ses_1')).toBe('');
    expect(agentOf('')).toBe('');
    expect(agentOf('?id=ses_1&agent=')).toBe('');
  });

  test('writes the same address back, escaped, and the main view without the parameter', () => {
    expect(viewHref('ses 1/x')).toBe('/session/?id=ses%201%2Fx');
    expect(viewHref('ses_1', 'ses_abc')).toBe('/session/?id=ses_1&agent=ses_abc');
    expect(agentOf(new URL(viewHref('ses_1', 'a&b=c'), 'http://x').search)).toBe('a&b=c');
  });

  test('only an ordinary click is the page’s to take over', () => {
    const click = { button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, defaultPrevented: false };
    expect(isPlainClick(click)).toBe(true);
    expect(isPlainClick({ ...click, ctrlKey: true })).toBe(false);
    expect(isPlainClick({ ...click, metaKey: true })).toBe(false);
    expect(isPlainClick({ ...click, shiftKey: true })).toBe(false);
    expect(isPlainClick({ ...click, button: 1 })).toBe(false);
    expect(isPlainClick({ ...click, defaultPrevented: true })).toBe(false);
  });
});
