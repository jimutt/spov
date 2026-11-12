import type { components } from "./api/types";
import { describeProblem } from "./problem";

/**
 * The People section's logic: what the admin typed, the list in order,
 * which acts the server would refuse, and what removal says. The server
 * decides every rule; this only mirrors it, so the page does not offer
 * an act that will be refused.
 *
 * @decision(DL-129) @decision(DL-128)
 */

export type AllowlistEntryView = components["schemas"]["AllowlistEntryView"];

/** EntryRef is what the add field names: an address or a GitHub login. */
export type EntryRef = { kind: "email"; email: string } | { kind: "github"; login: string };

/**
 * entryRef reads the add field. Text with "@" after its first character
 * is an address; anything else is a GitHub username, with or without
 * its leading "@". Empty text is null.
 *
 * @decision(DL-129)
 */
export function entryRef(text: string): EntryRef | null {
  const t = text.trim();
  if (!t) return null;
  if (t.indexOf("@", 1) > 0) return { kind: "email", email: t.toLowerCase() };
  return { kind: "github", login: t.replace(/^@/, "") };
}

/** GitHub's rule for a login, as the server checks it (DL-128). */
const LOGIN = /^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}$/;

/**
 * refProblem says why the field cannot be sent yet, or "" when it can.
 * The server checks again; this only keeps a malformed value out of a
 * URL path.
 *
 * @decision(DL-129) @decision(DL-128)
 */
export function refProblem(ref: EntryRef | null): string {
  if (!ref) return "Type a GitHub username or an address.";
  if (ref.kind === "github") {
    return LOGIN.test(ref.login)
      ? ""
      : `${JSON.stringify(ref.login)} is not a GitHub username: it has letters, digits or single hyphens, at most 39.`;
  }
  return /^[^\s@/\\?#]+@[^\s@/\\?#]+$/.test(ref.email) ? "" : `${JSON.stringify(ref.email)} is not an address.`;
}

/** refPath is the {entry} the API is sent for a ref. */
export function refPath(ref: EntryRef): string {
  return ref.kind === "github" ? `@${ref.login}` : ref.email;
}

/** entryPath is the {entry} that names a listed entry: its stored login, or its address. */
export function entryPath(e: AllowlistEntryView): string {
  return e.kind === "github" ? `@${e.github_login ?? ""}` : e.email;
}

/** entryLabel is how a row names its entry: "@login" or the address. */
export function entryLabel(e: AllowlistEntryView): string {
  return entryPath(e);
}

/** sortName is what the list sorts by after admin rights. */
function sortName(e: AllowlistEntryView): string {
  return (e.name || e.github_login || e.email || "").toLowerCase();
}

/**
 * sortEntries orders the list: administrators first, then by name
 * (the display name, else the login or address). A copy.
 *
 * @decision(DL-129)
 */
export function sortEntries(entries: readonly AllowlistEntryView[]): AllowlistEntryView[] {
  return [...entries].sort((a, b) => {
    if (a.admin !== b.admin) return a.admin ? -1 : 1;
    const by = sortName(a).localeCompare(sortName(b));
    return by !== 0 ? by : entryPath(a).localeCompare(entryPath(b));
  });
}

/**
 * guard says why taking admin rights away from an entry (by demoting or
 * removing it) would be refused, or "" when it would not: the org's last
 * admin entry, or the caller's only admin entry. Mirrors the server's
 * guardLastAdmin and losesOwnAdmin (DL-007, DL-128).
 */
function guard(e: AllowlistEntryView, entries: readonly AllowlistEntryView[]): string {
  if (!e.admin) return "";
  const admins = entries.filter((x) => x.admin);
  if (admins.length <= 1) return "This is the last administrator. Make someone else an administrator first.";
  if (e.you && !admins.some((x) => x !== e && x.you)) {
    return "This is your only administrator entry. Ask another administrator.";
  }
  return "";
}

/**
 * canRemove reports whether the server would accept removing e.
 *
 * @decision(DL-129)
 */
export function canRemove(e: AllowlistEntryView, entries: readonly AllowlistEntryView[]): boolean {
  return guard(e, entries) === "";
}

/**
 * canDemote reports whether the server would accept taking e's admin
 * rights away. An entry that is not an admin has none to take.
 *
 * @decision(DL-129)
 */
export function canDemote(e: AllowlistEntryView, entries: readonly AllowlistEntryView[]): boolean {
  return e.admin && guard(e, entries) === "";
}

/** blockedReason is the line shown beside a disabled act. */
export function blockedReason(e: AllowlistEntryView, entries: readonly AllowlistEntryView[]): string {
  return guard(e, entries);
}

/**
 * removalQuestion is the confirmation's text. It says what removal ends,
 * as DL-132 does it, unless another entry still covers the person, in
 * which case they keep their access and nothing of theirs is ended.
 *
 * @decision(DL-129) @decision(DL-132)
 */
export function removalQuestion(e: AllowlistEntryView): string {
  const who = entryLabel(e);
  const others = e.also_covered_by ?? [];
  if (others.length) {
    return `Remove ${who}? They keep their access through ${others.join(" and ")}, so nothing of theirs is ended.`;
  }
  if (!e.last_login_at) return `Remove ${who}? Nobody has signed in through it, so nothing else is ended.`;
  return (
    `Remove ${who}? This ends their browser sessions, cancels their jobs, stops their sessions, ` +
    `revokes their CLI tokens and deletes their stored credentials. Their history and templates are kept.`
  );
}

/**
 * isPartialRemoval reports a removal the server answered with a 500
 * after removing the entry: an off-boarding step failed, and the same
 * delete retries it.
 *
 * @decision(DL-129) @decision(DL-132)
 */
export function isPartialRemoval(error: unknown): boolean {
  return describeProblem(error).status === 500;
}

/**
 * refusalText is a refusal in the server's words: its detail, else its
 * title.
 *
 * @decision(DL-129)
 */
export function refusalText(error: unknown): string {
  const p = describeProblem(error);
  return p.detail || p.title;
}
