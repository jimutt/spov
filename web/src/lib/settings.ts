import { ApiError, type Problem } from "./api/client";
import type { components } from "./api/types";
import { limitLength, parseDuration } from "./format";
import {
  defaultClockLabel,
  keyNote as catalogueKeyNote,
  type ModelGroup,
} from "./jobform";

/**
 * The Settings page's logic, kept beside the component so tests can reach
 * it. The model picker reuses the job form's `modelGroups` and `keyNote`.
 *
 * @decision(DL-062)
 */

export type Me = components["schemas"]["MeView"];

// withoutVariant is re-exported from jobform.ts, not copied: it must agree
// with `prep.LookupModel` and `modelRef`.
//
// @decision(DL-062) @decision(DL-061)
export { modelGroups, withoutVariant } from "./jobform";
export type { ModelGroup, ModelProvider } from "./jobform";

/**
 * keyNote is the job form's `keyNote`. The OpenCode sign-in is further
 * down the same page.
 *
 * @decision(DL-062) @decision(DL-061) @decision(DL-015)
 */
export function keyNote(groups: ModelGroup[]): string {
  return catalogueKeyNote(groups, "below");
}

/**
 * NO_DEFAULT is the select's "let OpenCode choose" option. It is the empty
 * string because that is the value the API stores.
 *
 * @decision(DL-062) @decision(DL-014)
 */
export const NO_DEFAULT = "";

/**
 * defaultModelBody is what PATCH /api/v1/me is sent. An empty value is sent,
 * not omitted, because omitting it would leave the default unchanged. Do not
 * align this with the job form's omit rule; see DL-062.
 *
 * @decision(DL-062) @decision(DL-014)
 */
export function defaultModelBody(model: string): { default_model: string } {
  return { default_model: model.trim() };
}

/** What a save needs from the caller, so a test can supply it. */
export type PatchMe = (body: { default_model: string }) => Promise<{
  response: Response;
  data?: Me;
  error?: Problem;
}>;

/**
 * Field is one editable setting: what the server last reported (`stored`,
 * `undefined` until the profile arrives) and what the control holds.
 *
 * @decision(DL-062)
 */
export interface Field {
  stored: string | undefined;
  value: string;
}

/** A field nothing has been read into yet. */
export function emptyField(): Field {
  return { stored: undefined, value: "" };
}

/**
 * seedField folds an arriving profile into one field. Does nothing
 * before the profile has loaded, and follows it only while the reader
 * has not changed the value. Must return `prev` unchanged when nothing
 * moved, or the calling `$effect` reschedules itself forever.
 *
 * @decision(DL-062)
 */
export function seedField(loaded: boolean, current: string, prev: Field): Field {
  if (!loaded) return prev;
  const untouched = prev.stored === undefined || prev.value === prev.stored;
  const value = untouched ? current : prev.value;
  if (prev.stored === current && prev.value === value) return prev;
  return { stored: current, value };
}

/**
 * changed reports whether a save would send anything the server does
 * not already hold. A field the profile has not reached yet counts as
 * unchanged: nothing can differ from a value that was never read.
 *
 * @decision(DL-062)
 */
export function changed(...fields: Field[]): boolean {
  // Trimmed on both sides, because a save sends the trimmed value.
  return fields.some(
    (f) => f.stored !== undefined && f.value.trim() !== f.stored.trim(),
  );
}

/**
 * saveDefaultModel stores the choice and returns the profile the server now
 * holds. The page shows that answer rather than the value sent, since the
 * server trims and may refuse.
 *
 * @decision(DL-062) @decision(DL-014)
 */
export async function saveDefaultModel(
  model: string,
  patch: PatchMe,
): Promise<Me> {
  const r = await patch(defaultModelBody(model));
  if (!r.response.ok) throw new ApiError(r.response.status, r.error);
  if (!r.data) {
    throw new Error(
      "The server accepted the change but sent no profile back, so this page" +
        " cannot show what it now holds. Reload to see it.",
    );
  }
  return r.data;
}

/**
 * savedNote is what the page says after a successful save, read from the
 * server's value, so a clear gets a note of its own.
 *
 * @decision(DL-062)
 */
export function savedNote(me: Me): string {
  const model = me.default_model ?? "";
  return model
    ? `Every launch that names no model will run on ${model}.`
    : "Cleared. OpenCode chooses the model when a launch names none.";
}

/**
 * identityBody is what PATCH /api/v1/me is sent for the git identity. Both
 * halves are always sent, empty included, as in `defaultModelBody`; an empty
 * half falls back to the server's own identity.
 *
 * @decision(DL-062) @decision(DL-044)
 */
export function identityBody(
  name: string,
  email: string,
): { git_name: string; git_email: string } {
  return { git_name: name.trim(), git_email: email.trim() };
}

/** What an identity save needs from the caller, so a test can supply it. */
export type PatchIdentity = (body: {
  git_name: string;
  git_email: string;
}) => Promise<{ response: Response; data?: Me; error?: Problem }>;

/**
 * saveIdentity stores the git author and returns the profile the server now
 * holds, which is what the page shows.
 *
 * @decision(DL-062)
 */
export async function saveIdentity(
  name: string,
  email: string,
  patch: PatchIdentity,
): Promise<Me> {
  const r = await patch(identityBody(name, email));
  if (!r.response.ok) throw new ApiError(r.response.status, r.error);
  if (!r.data) {
    throw new Error(
      "The server accepted the change but sent no profile back, so this page" +
        " cannot show what it now holds. Reload to see it.",
    );
  }
  return r.data;
}

/**
 * savedIdentityNote is what the page says after a successful save. A
 * half-set identity gets its own sentence: `git.ResolveIdentity` fills
 * each half separately, so commits would carry this half beside the
 * server's other one.
 *
 * @decision(DL-062) @decision(DL-044)
 */
export function savedIdentityNote(me: Me): string {
  const name = (me.git_name ?? "").trim();
  const email = (me.git_email ?? "").trim();
  if (name && email) return `Sessions will author commits as ${name} <${email}>.`;
  if (!name && !email) {
    return "Cleared. Sessions will author commits with the server's own identity.";
  }
  const mine = name ? "name" : "e-mail address";
  const theirs = name ? "e-mail address" : "name";
  return (
    `Saved, but only half an identity: the halves are filled in separately, so` +
    ` commits will carry your ${mine} beside the server's ${theirs}.` +
    " Set both, or clear both."
  );
}

/**
 * GitAccess is a stored git access default. The page shows an empty one
 * as "repos", which is what the server makes of it.
 *
 * @decision(DL-043)
 */
export type GitAccess = NonNullable<Me["default_git_access"]>;

/** The value a git access select starts from. @decision(DL-043) */
export function accessValue(stored: string | undefined): GitAccess {
  return stored === "all" ? "all" : "repos";
}

/**
 * gitAccessBody is what PATCH /api/v1/me is sent for the two git access
 * defaults. Both are always sent, as the other settings are.
 *
 * @decision(DL-043) @decision(DL-062)
 */
export function gitAccessBody(
  session: string,
  job: string,
): { default_git_access: string; default_job_git_access: string } {
  return { default_git_access: session, default_job_git_access: job };
}

/** What a git access save needs from the caller. */
export type PatchAccess = (body: {
  default_git_access: string;
  default_job_git_access: string;
}) => Promise<{ response: Response; data?: Me; error?: Problem }>;

/**
 * saveGitAccess stores both defaults and returns the profile the server
 * now holds.
 *
 * @decision(DL-043)
 */
export async function saveGitAccess(session: string, job: string, patch: PatchAccess): Promise<Me> {
  const r = await patch(gitAccessBody(session, job));
  if (!r.response.ok) throw new ApiError(r.response.status, r.error);
  if (!r.data) {
    throw new Error(
      "The server accepted the change but sent no profile back, so this page" +
        " cannot show what it now holds. Reload to see it.",
    );
  }
  return r.data;
}

/**
 * savedAccessNote is what the page says after a git access save, read
 * from the server's values.
 *
 * @decision(DL-043)
 */
export function savedAccessNote(me: Me): string {
  const reach = (v: string | undefined) =>
    accessValue(v) === "all" ? "every repository your credentials reach" : "only their own repositories";
  return (
    `Sessions will reach ${reach(me.default_git_access)}, and jobs ${reach(me.default_job_git_access)}.` +
    (me.default_git_access === "all" || me.default_job_git_access === "all" ? ALL_ON_ORG_NOTE : "")
  );
}

/**
 * ALL_ON_ORG_NOTE says where a saved "all" does not apply.
 *
 * @decision(DL-043)
 */
export const ALL_ON_ORG_NOTE =
  " Where the credential is the deployment's rather than your own, all applies only if an admin allows it;" +
  " otherwise that platform gets the launch's own repositories, and the session page says so.";

/**
 * forgeTokenAdvice is what the Settings page says under a forge token:
 * where it goes, that spov cannot check what it reaches, and how to
 * narrow it. Empty for a model provider key.
 *
 * @decision(DL-043) @decision(DL-034) @decision(DL-041)
 */
export function forgeTokenAdvice(platform: string): string {
  const p = platform.trim();
  if (p === "" || p.startsWith("provider:")) return "";
  const every =
    "Every sandbox you start on this platform uses this token. Where spov's credential proxy runs, the agent never" +
    " sees it and it is used only for the session's repositories (unless you choose git access all), but spov" +
    " cannot check what the token itself" +
    " reaches, so a narrow token is a second limit.";
  if (p.startsWith("forgejo:")) {
    return `${every} Create it with "Specific repositories" and the write:repository and write:issue scopes: tea reads a repository's labels when it opens a pull request.`;
  }
  // @decision(DL-041) @decision(DL-034)
  if (p.startsWith("gitea:")) {
    return `${every} Create it with the write:repository and write:issue scopes: tea reads a repository's labels when it opens a pull request.`;
  }
  if (p.startsWith("gitlab:")) {
    return (
      `${every} Give it the api scope and a short expiry. A GitLab token reaches every project its account can,` +
      " and GitLab's GraphQL API names no project, so use an account with access to only the projects your" +
      " sessions should reach."
    );
  }
  if (p === "github") {
    return (
      `${every} Make it a fine-grained token limited to specific repositories: GitHub's GraphQL API names no` +
      " repository, so there it reaches whatever it was created to reach. It replaces the GitHub App token a" +
      " deployment would otherwise narrow to each session's repositories."
    );
  }
  return every;
}

/**
 * The default job durations the Settings page offers. Empty is the
 * built-in hour.
 *
 * @decision(DL-049)
 */
export const JOB_DURATIONS = ["30m", "1h", "2h", "3h", "4h"] as const;

/**
 * jobDurationValue is the select value for a stored default: the offered
 * duration it equals, or the stored text itself, or empty for none.
 *
 * @decision(DL-049)
 */
export function jobDurationValue(stored: string | undefined): string {
  const mins = stored ? parseDuration(stored) : null;
  if (mins === null) return "";
  const offered = JOB_DURATIONS.find((d) => parseDuration(d) === mins);
  if (offered) return offered;
  // Whole minutes read as the form writes them; anything finer is shown
  // as stored rather than rounded to a value that is not.
  return Number.isInteger(mins) ? limitLength(mins).replace(" ", "") : (stored ?? "");
}

/**
 * builtInDurationLabel names the empty choice: the built-in hour, or the
 * caller's limit when that is lower, as the job form says it.
 *
 * @decision(DL-049)
 */
export function builtInDurationLabel(limit: string | undefined): string {
  return defaultClockLabel(limit);
}

/**
 * jobDurationOptions is what the select offers: the durations within the
 * caller's limit, plus the current value if the limit has since dropped
 * below it, so the select can still show what is stored.
 *
 * @decision(DL-049)
 */
export function jobDurationOptions(limit: string | undefined, current: string): string[] {
  const max = limit ? parseDuration(limit) : null;
  const offered: string[] = JOB_DURATIONS.filter((d) => max === null || (parseDuration(d) ?? Infinity) <= max);
  if (current && !offered.includes(current)) offered.push(current);
  return offered;
}

/** What a default job duration save needs from the caller. */
export type PatchJobDuration = (body: {
  default_job_duration: string;
}) => Promise<{ response: Response; data?: Me; error?: Problem }>;

/**
 * saveJobDuration stores the default job duration, empty clearing it, and
 * returns the profile the server now holds.
 *
 * @decision(DL-049) @decision(DL-062)
 */
export async function saveJobDuration(value: string, patch: PatchJobDuration): Promise<Me> {
  const r = await patch({ default_job_duration: value });
  if (!r.response.ok) throw new ApiError(r.response.status, r.error);
  if (!r.data) {
    throw new Error(
      "The server accepted the change but sent no profile back, so this page" +
        " cannot show what it now holds. Reload to see it.",
    );
  }
  return r.data;
}

/**
 * savedJobDurationNote is what the page says after the save, read from
 * the server's value and the caller's limit.
 *
 * @decision(DL-049)
 */
export function savedJobDurationNote(me: Me): string {
  const mins = me.default_job_duration ? parseDuration(me.default_job_duration) : null;
  const max = me.quota?.max_job_duration ? parseDuration(me.quota.max_job_duration) : null;
  const wanted = mins ?? 60;
  const said = mins === null ? "an hour, the built-in default" : limitLength(mins);
  const capped = max !== null && max < wanted ? ` Your limit is ${limitLength(max)}, so they get that.` : "";
  return `Jobs that do not say otherwise will run for up to ${said} unattended.${capped}`;
}

/**
 * credentialFacts is what the Settings list says about a stored
 * credential: whose it is, and where the forge said, its scopes and
 * when it expires. spov never renews a stored token, so a close expiry
 * is counted in days.
 *
 * @decision(DL-041)
 */
export function credentialFacts(
  cred: { verified_as?: string; scopes?: string[] | null; expires_at?: string },
  now: number,
): string {
  const parts: string[] = [];
  if (cred.verified_as) parts.push(`Verified as ${cred.verified_as}`);
  if (cred.scopes && cred.scopes.length > 0) parts.push(`scopes ${cred.scopes.join(", ")}`);
  if (cred.expires_at) {
    const at = Date.parse(cred.expires_at);
    if (!Number.isNaN(at)) {
      const day = new Date(at).toISOString().slice(0, 10);
      const days = Math.floor((at - now) / 86_400_000);
      if (at <= now) parts.push(`expired ${day}`);
      else if (days < 30) parts.push(`expires ${day}, in ${days} ${days === 1 ? "day" : "days"}`);
      else parts.push(`expires ${day}`);
    }
  }
  return parts.join(" · ");
}
