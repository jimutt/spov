/**
 * The transcript draws a window of its newest blocks. While the page
 * follows the tail the window slides with it; once the reader leaves
 * the end it stays where it starts, so nothing is taken out from above
 * them, and "Show earlier" moves its start back. Plain data in and out,
 * so vitest reaches it.
 *
 * @decision(DL-070)
 */

/** WindowSize is how many blocks the window holds while it slides, and
 *  how many more each "Show earlier" adds. */
export const WindowSize = 200;

/**
 * Start is where a held window begins: the key of its first block, and
 * that block's index when it was taken, used only when the key is gone.
 * Undefined means the window slides: the newest WindowSize blocks.
 */
export type Start = { key: string; index: number } | undefined;

export interface Windowed<T> {
  /** The blocks to draw, oldest first. */
  shown: T[];
  /** How many blocks come before the first one drawn. */
  hidden: number;
}

/** startIndex is where the window begins in blocks. */
function startIndex(blocks: readonly { key: string }[], start: Start, size: number): number {
  if (start === undefined) return Math.max(0, blocks.length - size);
  const at = blocks.findIndex((b) => b.key === start.key);
  return Math.min(at >= 0 ? at : start.index, Math.max(0, blocks.length - 1));
}

/** windowOf is the part of blocks to draw. */
export function windowOf<T extends { key: string }>(blocks: readonly T[], start: Start, size: number = WindowSize): Windowed<T> {
  const from = blocks.length === 0 ? 0 : startIndex(blocks, start, size);
  return { shown: from === 0 ? (blocks as T[]) : blocks.slice(from), hidden: from };
}

/**
 * held is the window's start once the reader leaves the end: where it
 * begins now, so the next growth adds below it and takes nothing away,
 * however few blocks it holds yet. A window already held keeps its start.
 */
export function held(blocks: readonly { key: string }[], start: Start, size: number = WindowSize): Start {
  if (start !== undefined || blocks.length === 0) return start;
  const from = startIndex(blocks, undefined, size);
  return { key: blocks[from].key, index: from };
}

/** earlier is the start one WindowSize further back. */
export function earlier(blocks: readonly { key: string }[], start: Start, size: number = WindowSize): Start {
  if (blocks.length === 0) return start;
  const from = Math.max(0, startIndex(blocks, start, size) - size);
  return { key: blocks[from].key, index: from };
}

/** earlierLabel is the control's text, with how many blocks are not drawn. */
export function earlierLabel(hidden: number): string {
  return hidden === 1 ? "Show earlier (1 more)" : `Show earlier (${hidden} more)`;
}
