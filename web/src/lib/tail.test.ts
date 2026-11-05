import { describe, expect, test } from 'vitest';
import { NearBottom, Tail, atBottom } from './tail';

/**
 * The latch's tests. They are the reason it is a module rather than
 * inline in the component: the behaviour that matters here is a
 * sequence — read, grow, scroll up, grow again — and a component test
 * cannot express one without a real scrolling viewport.
 *
 * @decision(DL-057)
 */

/** A scroller with `height` visible out of `content`, scrolled to `at`. */
function scroller(at: number, content = 5000, height = 800) {
  return { scrollTop: at, scrollHeight: content, clientHeight: height };
}

/** The position that is exactly the end. */
function end(content = 5000, height = 800) {
  return content - height;
}

describe('being at the bottom', () => {
  test('a fraction short of the end still counts', () => {
    // Sub-pixel layout and browser scroll anchoring both leave
    // scrollTop a hair short of the maximum. An exact comparison
    // reports "not at the bottom" for a reader who plainly is, and the
    // page then never follows for them at all.
    expect(atBottom(scroller(end() - 0.5))).toBe(true);
  });

  test('a screen up the page does not count', () => {
    expect(atBottom(scroller(end() - 800))).toBe(false);
  });

  test('content too short to scroll is at the bottom', () => {
    // Nothing to have scrolled away from, so the page should follow
    // the first arriving events rather than wait for a scroll that
    // cannot happen.
    expect(atBottom({ scrollTop: 0, scrollHeight: 400, clientHeight: 400 })).toBe(true);
  });

  test('the slack is more than a line and less than a screen', () => {
    // Stated as a range rather than a value: the point is that it
    // survives rounding without swallowing a deliberate scroll-up.
    expect(NearBottom).toBeGreaterThan(4);
    expect(NearBottom).toBeLessThan(200);
  });
});

describe('following the tail', () => {
  test('a page opens following, so it lands at the end', () => {
    // The transcript arrives after mount, so the latch has to be armed
    // before there is anything to scroll — otherwise the first render
    // leaves the reader at the top of a long run.
    const tail = new Tail();
    expect(tail.following).toBe(true);
    expect(tail.onGrow(scroller(0, 5000))).toBe(5000);
  });

  test('scrolling up releases the latch and keeps it released', () => {
    const tail = new Tail();
    tail.onScroll(scroller(end()));
    tail.onScroll(scroller(end() - 900));
    expect(tail.following).toBe(false);
    // The event that arrives while the reader is up the page must not
    // yank them back down. This is the whole point of the latch, and
    // the naive "scroll on every append" version fails here.
    expect(tail.onGrow(scroller(end() - 900, 6000))).toBeUndefined();
    expect(tail.onGrow(scroller(end() - 900, 7000))).toBeUndefined();
  });

  test('scrolling back to the end resumes following', () => {
    const tail = new Tail();
    tail.onScroll(scroller(end()));
    tail.onScroll(scroller(end() - 900));
    expect(tail.following).toBe(false);
    tail.onScroll(scroller(end()));
    expect(tail.following).toBe(true);
    expect(tail.onGrow(scroller(end(), 6000))).toBe(6000);
  });

  test('a reader at the end is carried along by new events', () => {
    const tail = new Tail();
    tail.onScroll(scroller(end(5000), 5000));
    expect(tail.onGrow(scroller(end(5000), 6000))).toBe(6000);
  });

  test('the way back re-arms the latch', () => {
    const tail = new Tail();
    tail.onScroll(scroller(end()));
    tail.onScroll(scroller(0));
    expect(tail.following).toBe(false);
    expect(tail.resume(scroller(0, 6000))).toBe(6000);
    expect(tail.following).toBe(true);
  });

  test('following survives a run of arriving events', () => {
    // The sequence a live job actually produces: an event lands, the
    // page follows it, the browser then delivers the scroll that the
    // follow caused, and another event lands. The latch has to still be
    // armed at the end — a design that tried to distinguish the page's
    // own scrolls from the reader's would stop following after the
    // first one it followed, because the scroll event arrives on a
    // later frame than the call that caused it.
    const tail = new Tail();
    let content = 5000;
    for (let i = 0; i < 3; i++) {
      content += 1000;
      const grown = scroller(end(content - 1000), content);
      expect(tail.onGrow(grown), `event ${i}`).toBe(content);
      // The browser's scroll event, now at the new end.
      tail.onScroll(scroller(end(content), content));
    }
    expect(tail.following).toBe(true);
    // And a reader scrolling up mid-run still stops it.
    tail.onScroll(scroller(end(content) - 900, content));
    expect(tail.onGrow(scroller(end(content) - 900, content + 1000))).toBeUndefined();
  });

  test('growth that leaves the reader short of the end does not release', () => {
    // A fast replay: the content grows past the end before the page has
    // followed it, and a scroll event lands first at the same position.
    // @decision(DL-070)
    const tail = new Tail();
    tail.onScroll(scroller(end(5000), 5000));
    tail.onScroll(scroller(end(5000), 9000));
    expect(tail.following).toBe(true);
    expect(tail.onGrow(scroller(end(5000), 9000))).toBe(9000);
  });

  test('the first scroll event only records where the scroller is', () => {
    // Scroll anchoring while a replay arrives, before the page has
    // followed anything: nothing to compare with, so not the reader.
    // @decision(DL-070)
    const tail = new Tail();
    tail.onScroll(scroller(0, 9000));
    expect(tail.following).toBe(true);
  });

  test('blocks dropped from above do not release', () => {
    // The transcript's window slides: blocks leave the top and the
    // content shrinks, which can clamp the position lower.
    // @decision(DL-070)
    const tail = new Tail();
    tail.onScroll(scroller(end(9000), 9000));
    tail.onScroll(scroller(end(7000), 7000));
    expect(tail.following).toBe(true);
  });

  test('one step up after growth that fired no scroll event releases', () => {
    // PageUp or a jump to a find-in-page match is a single event, and
    // the content grew since the last one without a scroll.
    // @decision(DL-070)
    const tail = new Tail();
    tail.onScroll(scroller(end(9000), 9000));
    tail.onScroll(scroller(end(9000) - 800, 9600));
    expect(tail.following).toBe(false);
  });

  test('release stops following until the end is reached again', () => {
    // @decision(DL-070)
    const tail = new Tail();
    tail.release();
    expect(tail.onGrow(scroller(0, 9000))).toBeUndefined();
    tail.onScroll(scroller(end(9000), 9000));
    expect(tail.following).toBe(true);
  });

  test('a reader scrolling up after layout has settled releases', () => {
    // @decision(DL-070)
    const tail = new Tail();
    tail.onScroll(scroller(end(9000), 9000));
    tail.onScroll(scroller(end(9000) - 400, 9000));
    expect(tail.following).toBe(false);
    // Content growing below while they read keeps it released.
    tail.onScroll(scroller(end(9000) - 400, 9500));
    expect(tail.following).toBe(false);
  });
});

