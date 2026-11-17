import type { components } from "./api/types";
import { describeProblem } from "./problem";

/**
 * The Service accounts section's logic: what the admin typed, which
 * values may reach a URL path, the list in order, the requests, and what
 * each row says. The server decides every rule; this only mirrors it, so
 * a malformed value is never sent.
 *
 * @decision(DL-142) @decision(DL-140) @decision(DL-141)
 */

export type ServiceAccountView = components["schemas"]["ServiceAccountView"];
export type ServiceTokenView = components["schemas"]["ServiceTokenView"];
export type IssuedServiceToken = components["schemas"]["IssuedServiceToken"];
export type CreateAccountBody = components["schemas"]["CreateServiceAccountInputBody"];
export type PatchAccountBody = components["schemas"]["PatchServiceAccountInputBody"];
export type CreateTokenBody = components["schemas"]["CreateServiceTokenInputBody"];

/** The server's shape of an account's name (DL-140). */
const NAME = /^[a-z][a-z0-9-]{1,30}[a-z0-9]$/;

/** A ULID, the shape of every token ID the server issues. */
const ULID = /^[0-9A-HJKMNP-TV-Z]{26}$/;

/** The longest label a token may have, in characters (DL-141). */
export const MaxLabel = 64;
/** The longest lifetime a token may name, in days (DL-141). */
export const MaxDays = 1095;
/** The lifetime the server gives a token that names none (DL-141). */
export const DefaultDays = 365;

/**
 * nameProblem says why the create field cannot be sent, or "" when it
 * can. The server checks again; this keeps the field's text out of a URL
 * path that could reach another route.
 *
 * @decision(DL-142)
 */
export function nameProblem(text: string): string {
  const t = text.trim();
  if (!t) return "Name the account.";
  return NAME.test(t)
    ? ""
    : `${JSON.stringify(t)} is not a service account name: 3 to 32 lower-case letters, digits and hyphens, ` +
        `starting with a letter and not ending with a hyphen.`;
}

/**
 * accountPath is the {name} an account is addressed by, or null for a
 * name the server could not have given, which is never sent.
 *
 * @decision(DL-142)
 */
export function accountPath(name: string): string | null {
  return NAME.test(name) ? name : null;
}

/**
 * tokenPath is the {id} a token is addressed by, or null for anything
 * that is not a ULID, which is never sent.
 *
 * @decision(DL-142)
 */
export function tokenPath(id: string): string | null {
  return ULID.test(id) ? id : null;
}

/** isDeleted reports an account that has been deleted; its name stays taken. */
export function isDeleted(a: ServiceAccountView): boolean {
  return Boolean(a.disabled_at);
}

/**
 * sortAccounts orders the list: accounts that are not deleted first,
 * then the deleted ones, each by name. A copy.
 *
 * @decision(DL-142)
 */
export function sortAccounts(accounts: readonly ServiceAccountView[]): ServiceAccountView[] {
  return [...accounts].sort((a, b) => {
    if (isDeleted(a) !== isDeleted(b)) return isDeleted(a) ? 1 : -1;
    return a.name.localeCompare(b.name);
  });
}

/**
 * createRequest is the create's body: the name, and an author field only
 * when it was filled in, so the server's defaults apply to the others.
 *
 * @decision(DL-142)
 */
export function createRequest(name: string, gitName: string, gitEmail: string): CreateAccountBody {
  const body: CreateAccountBody = { name: name.trim() };
  if (gitName.trim()) body.git_name = gitName.trim();
  if (gitEmail.trim()) body.git_email = gitEmail.trim();
  return body;
}

/**
 * authorPatch is the PATCH body for an inline edit: only the fields that
 * changed, or null when none did.
 *
 * @decision(DL-142)
 */
export function authorPatch(a: ServiceAccountView, gitName: string, gitEmail: string): PatchAccountBody | null {
  const body: PatchAccountBody = {};
  if (gitName.trim() !== a.git_name) body.git_name = gitName.trim();
  if (gitEmail.trim() !== a.git_email) body.git_email = gitEmail.trim();
  return Object.keys(body).length ? body : null;
}

/**
 * dayText names a moment's day: today, yesterday or tomorrow, else the
 * date, with the year when it is not this year's.
 */
export function dayText(iso: string, now: Date): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const a = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const b = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const days = Math.round((b.getTime() - a.getTime()) / 86_400_000);
  if (days === 0) return "today";
  if (days === 1) return "yesterday";
  if (days === -1) return "tomorrow";
  const year = d.getFullYear() === now.getFullYear() ? undefined : "numeric";
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year });
}

/** lastUsedText is when an account or a token was last used. */
export function lastUsedText(iso: string | undefined, now: Date): string {
  return iso ? `Last used ${dayText(iso, now)}` : "Never used";
}

/** tokenCountText is how many usable tokens an account holds. */
export function tokenCountText(n: number): string {
  if (n === 0) return "No usable tokens";
  return n === 1 ? "1 usable token" : `${n} usable tokens`;
}

/**
 * deletedText is a deleted account's mark.
 *
 * @decision(DL-142)
 */
export function deletedText(a: ServiceAccountView, now: Date): string {
  return a.disabled_at ? `Deleted ${dayText(a.disabled_at, now)}` : "";
}

/**
 * Lifetime is the token form's choice: the server's default, a number
 * of days, or no expiry.
 */
export type Lifetime = "default" | "days" | "never";

/**
 * labelProblem says why a token's label cannot be sent, or "".
 *
 * @decision(DL-142)
 */
export function labelProblem(label: string): string {
  const t = label.trim();
  if (!t) return "Name the token, such as the pipeline that will hold it.";
  return [...t].length > MaxLabel ? `A token's label is at most ${MaxLabel} characters.` : "";
}

/**
 * lifetimeProblem says why a lifetime cannot be sent, or "".
 *
 * @decision(DL-142)
 */
export function lifetimeProblem(choice: Lifetime, days: string): string {
  if (choice !== "days") return "";
  const t = days.trim();
  const n = Number(t);
  return /^\d+$/.test(t) && n >= 1 && n <= MaxDays ? "" : `A lifetime is 1 to ${MaxDays} days.`;
}

/**
 * tokenRequest is the token create's body. The default lifetime is left
 * out, so the server's applies.
 *
 * @decision(DL-142)
 */
export function tokenRequest(label: string, choice: Lifetime, days: string): CreateTokenBody {
  const body: CreateTokenBody = { name: label.trim() };
  if (choice === "days") body.expires_in_days = Number(days.trim());
  if (choice === "never") body.no_expiry = true;
  return body;
}

/** issuerText names who issued a token, or "unknown" when the server gives no label. */
export function issuerText(t: ServiceTokenView): string {
  return t.issued_by?.label || "unknown";
}

/**
 * expiryText is when a token expires, has expired, or that it never
 * does. An expired token is still listed until it is revoked.
 *
 * @decision(DL-142)
 */
export function expiryText(t: ServiceTokenView, now: Date): string {
  if (!t.expires_at) return "Never expires";
  const at = Date.parse(t.expires_at);
  const day = dayText(t.expires_at, now);
  return !Number.isNaN(at) && at <= now.getTime() ? `Expired ${day}` : `Expires ${day}`;
}

/**
 * deletionQuestion is the delete confirmation's text. It says what the
 * server's delete ends for a service account (DL-140).
 *
 * @decision(DL-142) @decision(DL-140)
 */
export function deletionQuestion(a: ServiceAccountView): string {
  return (
    `Delete ${a.name}? This cancels its jobs, stops its sessions, revokes its tokens and deletes its ` +
    `stored credentials. Its history is kept, and the name cannot be used again.`
  );
}

/**
 * isPartialDeletion reports a delete the server answered with a 500
 * after disabling the account: an off-boarding step failed, and the
 * same delete retries it.
 *
 * @decision(DL-142) @decision(DL-140)
 */
export function isPartialDeletion(error: unknown): boolean {
  return describeProblem(error).status === 500;
}

/**
 * copyText writes text to the clipboard and reports whether it could.
 * A missing clipboard, or one that refuses, is false.
 *
 * @decision(DL-142)
 */
export async function copyText(text: string, clipboard: Pick<Clipboard, "writeText"> | undefined): Promise<boolean> {
  if (!clipboard) return false;
  try {
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
