import { describe, expect, test } from 'vitest';
import { PageSize, appendPage, merged, olderLabel, sessionsCursor } from './paging';

const rows = (...ids: string[]) => ids.map((id) => ({ id }));

// @decision(DL-060)
describe('paging', () => {
  test('the sessions cursor is the oldest of a full newest page, not of the live rows after it', () => {
    const page = Array.from({ length: PageSize }, (_, i) => ({ id: `s${String(100 - i).padStart(3, '0')}` }));
    const withLive = [...page, { id: 's001' }];
    expect(sessionsCursor(withLive)).toBe(page[PageSize - 1].id);
    expect(sessionsCursor(page.slice(0, PageSize - 1))).toBe('');
  });

  test('an appended page skips rows already appended', () => {
    expect(appendPage(rows('a'), rows('b', 'z', 'a', 'y'))).toEqual(rows('a', 'b', 'z', 'y'));
  });

  // A live session older than the newest page is in the stream's frame
  // and in an older page. When it stops it leaves the frame, and the
  // appended copy must still draw it.
  test('a row the newest page drops is still drawn from the appended one', () => {
    const appended = appendPage([], rows('old-live', 'x'));
    expect(merged(rows('n1', 'old-live'), appended).map((r) => r.id)).toEqual(['n1', 'old-live', 'x']);
    expect(merged(rows('n1'), appended).map((r) => r.id)).toEqual(['n1', 'old-live', 'x']);
  });

  test('the newest page wins for a row in both', () => {
    const newest = [{ id: 'b', v: 'new' }];
    const appended = [{ id: 'b', v: 'old' }, { id: 'a', v: 'old' }];
    expect(merged(newest, appended)).toEqual([{ id: 'b', v: 'new' }, { id: 'a', v: 'old' }]);
  });

  test('the control says how many remain once it knows', () => {
    expect(olderLabel(undefined, 50)).toBe('Show older');
    expect(olderLabel(127, 50)).toBe('Show older (77 more)');
    expect(olderLabel(50, 50)).toBe('Show older');
  });
});
