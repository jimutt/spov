import type { Entry, SubagentEntry, Timelines } from "./timeline";
import type { SubagentState } from "./subagent";

/**
 * A subagent's own view on the session page: which view the address
 * names, the tree of subagents the page holds, and where Previous, Next
 * and the way back lead. Plain data in and out, so vitest reaches it.
 *
 * The tree is read from the cards `buildTimelines` already drew, so the
 * labels and states are the ones the card says, and nothing here asks
 * for a request or a sandbox. A subagent is a query parameter on the
 * session page, never a route (DL-057, DL-113).
 *
 * @decision(DL-113)
 */

/** One subagent in the tree: what its card says, and where it hangs. */
export interface SubagentNode {
  /** The child conversation's OpenCode id. */
  id: string;
  /** Its parent's id, "" when the main conversation started it. */
  parent: string;
  /** How many levels down the main conversation it is; 0 is a direct child. */
  depth: number;
  label: string;
  agent: string;
  state: SubagentState;
  /** How many tool calls it made, as its card counts them. */
  calls: number;
  /** Where its card is drawn in its parent's list: later is newer. */
  at: number;
  seq: number;
}

export interface SubagentTree {
  /** Every subagent, by id. */
  byId: ReadonlyMap<string, SubagentNode>;
  /** Children per parent id, in creation order; "" is the main conversation. */
  under: ReadonlyMap<string, readonly string[]>;
  /** Every subagent, each followed by its own, for the sidebar's list. */
  order: readonly SubagentNode[];
}

/**
 * subagentTree reads the subagents out of the cards of the main list and
 * of each child's list. A card with no child behind it (a call still
 * delegating) is not a subagent yet and is left out. A child lists its
 * siblings in the order its parent's list draws their cards, which is
 * the order their calls were made, and an id reached twice or through a
 * loop is kept where it was first met.
 */
export function subagentTree(t: Pick<Timelines, "entries" | "children">): SubagentTree {
  const byId = new Map<string, SubagentNode>();
  const under = new Map<string, string[]>();
  const walk = (owner: string, list: readonly Entry[] | undefined, depth: number) => {
    for (const e of list ?? []) {
      if (e.kind !== "subagent" || !e.id || byId.has(e.id)) continue;
      byId.set(e.id, nodeOf(e, owner, depth));
      const mine = under.get(owner);
      if (mine) mine.push(e.id);
      else under.set(owner, [e.id]);
      walk(e.id, t.children.get(e.id), depth + 1);
    }
  };
  walk("", t.entries, 0);
  const order: SubagentNode[] = [];
  const flat = (owner: string) => {
    for (const id of under.get(owner) ?? []) {
      order.push(byId.get(id)!);
      flat(id);
    }
  };
  flat("");
  return { byId, under, order };
}

function nodeOf(e: SubagentEntry, parent: string, depth: number): SubagentNode {
  return { id: e.id, parent, depth, label: e.title || "Subagent", agent: e.agent, state: e.state, calls: e.calls, at: e.at, seq: e.seq };
}

/** Where a subagent is among its siblings. */
export interface Siblings {
  /** 1-based place among the children of one parent. */
  place: number;
  total: number;
  previous?: SubagentNode;
  next?: SubagentNode;
}

/** siblingsOf places a subagent among the children of its parent. */
export function siblingsOf(tree: SubagentTree, id: string): Siblings | undefined {
  const node = tree.byId.get(id);
  if (!node) return undefined;
  const ids = tree.under.get(node.parent) ?? [];
  const at = ids.indexOf(id);
  return {
    place: at + 1,
    total: ids.length,
    previous: at > 0 ? tree.byId.get(ids[at - 1]) : undefined,
    next: at < ids.length - 1 ? tree.byId.get(ids[at + 1]) : undefined,
  };
}

/** trailTo is a subagent's ancestors from the top, then itself. */
export function trailTo(tree: SubagentTree, id: string): SubagentNode[] {
  const out: SubagentNode[] = [];
  for (let n = tree.byId.get(id); n; n = tree.byId.get(n.parent)) out.unshift(n);
  return out;
}

/** scopeOf is a subagent and every one under it, whose asks its view counts. */
export function scopeOf(tree: SubagentTree, id: string): string[] {
  return tree.byId.has(id) ? [id, ...(tree.under.get(id) ?? []).flatMap((c) => scopeOf(tree, c))] : [];
}

/**
 * askingSessions is the conversations with a question waiting on the
 * reader. Asks stay in the main list whichever conversation raised them,
 * so that list is the one to read.
 */
export function askingSessions(entries: readonly Entry[], states: ReadonlyMap<string, { status: string }>): Set<string> {
  const out = new Set<string>();
  for (const e of entries) {
    if (e.kind === "event" && e.ask && e.session && states.get(e.ask.id)?.status === "pending") out.add(e.session);
  }
  return out;
}

/** The sidebar's list of subagents, split into what is live and what is over. */
export interface SidebarList {
  /** Live subagents, and the ones kept for context, nested in tree order. */
  live: SubagentNode[];
  /** The rest, which are over, most recently created first. */
  finished: SubagentNode[];
  /** The heading's counts: "2 running, 18 done"; empty with no subagents. */
  counts: string;
}

const Live: ReadonlySet<SubagentState> = new Set(["starting", "running"]);

/**
 * sidebarList keeps a long session's finished subagents out of the way:
 *
 * - a subagent is live when it is starting or running, or when it or one
 *   under it has a question waiting on the reader (`asking`);
 * - the live ones are always listed, and so is the one whose view is open
 *   (`viewing`) even when it is over, and each of their ancestors, so the
 *   nesting reads; they stay in tree order;
 * - every other subagent is finished, listed once, newest first, behind
 *   one disclosure, never also among the live ones.
 *
 * The counts say how many are in each state, whichever part they sit in.
 */
export function sidebarList(tree: SubagentTree, viewing: string, asking: ReadonlySet<string> = new Set()): SidebarList {
  const shown = new Set<string>();
  const keep = (id: string) => {
    for (let n = tree.byId.get(id); n && !shown.has(n.id); n = tree.byId.get(n.parent)) shown.add(n.id);
  };
  for (const n of tree.order) {
    if (Live.has(n.state) || scopeOf(tree, n.id).some((id) => asking.has(id))) keep(n.id);
  }
  if (viewing) keep(viewing);
  const live = tree.order.filter((n) => shown.has(n.id));
  const finished = tree.order.filter((n) => !shown.has(n.id)).sort((a, b) => b.at - a.at || b.seq - a.seq);
  return { live, finished, counts: countsLine(tree.order) };
}

/** countsLine says how many subagents are in each state, the empty ones left out. */
export function countsLine(nodes: readonly SubagentNode[]): string {
  const n = { running: 0, done: 0, failed: 0, stopped: 0 };
  for (const node of nodes) {
    if (Live.has(node.state)) n.running++;
    else n[node.state as "done" | "failed" | "stopped"]++;
  }
  return (["running", "done", "failed", "stopped"] as const)
    .filter((k) => n[k] > 0)
    .map((k) => `${n[k]} ${k}`)
    .join(", ");
}

/** agentOf is the subagent a query string names, "" for the main conversation. */
export function agentOf(search: string): string {
  return new URLSearchParams(search).get("agent") ?? "";
}

/**
 * viewHref is the address of a view of one session: the main
 * conversation's, or a subagent's. The reference is the one the page was
 * opened with, so a link from a subagent's view never names the session
 * differently.
 */
export function viewHref(ref: string, agent = ""): string {
  const base = `/session/?id=${encodeURIComponent(ref)}`;
  return agent ? `${base}&agent=${encodeURIComponent(agent)}` : base;
}

/**
 * isPlainClick says a click on a link is an ordinary one, which the page
 * may take over to switch view without loading it again. A modified
 * click, a middle click and a link meant for another tab are the
 * browser's.
 */
export function isPlainClick(ev: {
  button: number;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
  defaultPrevented: boolean;
}): boolean {
  return ev.button === 0 && !ev.metaKey && !ev.ctrlKey && !ev.shiftKey && !ev.altKey && !ev.defaultPrevented;
}
