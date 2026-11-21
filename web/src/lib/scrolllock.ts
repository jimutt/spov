/**
 * The page's scroll lock, shared by every sheet and panel drawn over it.
 * A count rather than each overlay saving and restoring the body's
 * overflow: two overlays that close in either order would otherwise
 * restore each other's `hidden` and leave the page unscrollable.
 *
 * @decision(DL-167) @decision(DL-106)
 */
let holders = 0;
let before = "";

/** lockScroll keeps the page still until the returned release is called. */
export function lockScroll(): () => void {
  if (holders++ === 0) {
    before = document.body.style.overflow;
    document.body.style.overflow = "hidden";
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    if (--holders === 0) document.body.style.overflow = before;
  };
}
