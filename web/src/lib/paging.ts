/**
 * Paging for the sessions and jobs pages: the newest page as the page
 * already reads it, and older pages appended on request, read once.
 * Plain data in and out, so vitest reaches it.
 *
 * @decision(DL-060)
 */

/** PageSize is how many rows a page of either list holds. */
export const PageSize = 50;

/**
 * sessionsCursor is where "Show older" starts on the sessions page: the
 * oldest of the newest page the stream sent, which leads every frame,
 * before the older live sessions after it (DL-012). Empty when that
 * page was not full, since then nothing older exists.
 */
export function sessionsCursor(streamed: readonly { id: string }[]): string {
  return streamed.length >= PageSize ? streamed[PageSize - 1].id : "";
}

/**
 * appendPage adds an older page to the rows already appended, skipping
 * any row already among them. A row the newest page also holds is kept
 * here too: `merged` draws the newest page's copy while it has one, and
 * this one takes over once it drops out, as a live session does from
 * the stream when it stops.
 */
export function appendPage<T extends { id: string }>(appended: readonly T[], page: readonly T[]): T[] {
  const have = new Set(appended.map((r) => r.id));
  return [...appended, ...page.filter((r) => !have.has(r.id))];
}

/**
 * merged is what the page draws: the newest rows, then the appended
 * ones the newest do not already hold.
 */
export function merged<T extends { id: string }>(newest: readonly T[], appended: readonly T[]): T[] {
  const have = new Set(newest.map((r) => r.id));
  return [...newest, ...appended.filter((r) => !have.has(r.id))];
}

/**
 * olderLabel is the control's text: how many remain when the total is
 * known, a plain "Show older" before then.
 */
export function olderLabel(total: number | undefined, shown: number): string {
  if (total === undefined) return "Show older";
  const left = total - shown;
  return left > 0 ? `Show older (${left} more)` : "Show older";
}
