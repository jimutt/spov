/**
 * Following the end of a growing transcript: a latch, not a scroll. It
 * sticks to the bottom only while the reader is already there, releases
 * the moment they scroll up, and re-arms when they return to the end.
 * Must not re-anchor on DL-058's five-minute stream reopen.
 *
 * @decision(DL-057) @decision(DL-058)
 */

/**
 * NearBottom is how far from the end still counts as "at the end", in
 * CSS pixels. Not zero: sub-pixel layout and browser scroll anchoring
 * leave `scrollTop` a fraction short of the maximum, so an exact
 * comparison would report a reader at the bottom as not there.
 */
export const NearBottom = 64;

/** Scroller is the part of a scrolling element this needs. Narrow so a
 *  test can pass a plain object rather than a DOM node. */
export interface Scroller {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/**
 * atBottom reports whether a scroller is at (or within NearBottom of)
 * its end. An element too short to scroll is at the bottom by
 * definition, since its scrollHeight and clientHeight are equal.
 */
export function atBottom(el: Scroller, within: number = NearBottom): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= within;
}

/**
 * Tail is the latch.
 *
 * Call `onScroll` from the scroll handler and `onGrow` after the
 * content changes. `following` is whether the page should currently
 * stick to the end.
 */
export class Tail {
  /** Whether the page is currently sticking to the end. */
  following = true;

  #within: number;

  constructor(within: number = NearBottom) {
    this.#within = within;
  }

  /**
   * onScroll records where the reader is. Scrolling up releases the
   * latch; scrolling back to the end re-arms it.
   *
   * Called for scrolls the page causes as well as ones the reader
   * causes, deliberately: a programmatic scroll to the end leaves the
   * reader at the end, which is what `following` already says. Do not
   * suppress the handler around a programmatic scroll instead — the
   * scroll event arrives on a later frame than the call that caused it.
   */
  onScroll(el: Scroller): void {
    this.following = atBottom(el, this.#within);
  }

  /**
   * onGrow reports where the content should be scrolled to now that it
   * has changed, or `undefined` to leave it alone.
   *
   * Returning a position rather than performing the scroll keeps this
   * testable without a DOM, and keeps the one place that touches the
   * element in the component.
   */
  onGrow(el: Scroller): number | undefined {
    if (!this.following) return undefined;
    return el.scrollHeight;
  }

  /**
   * resume is the reader taking the offered way back. It re-arms the
   * latch and asks for the end.
   */
  resume(el: Scroller): number {
    this.following = true;
    return el.scrollHeight;
  }
}
