import { ApiError, type Problem } from "./api/client";
import type { components } from "./api/types";
import { limitLength, parseDuration } from "./format";

/**
 * The job-creation form's logic, beside the component rather than in
 * it, so a test can reach it.
 *
 * @decision(DL-061)
 */

export type ModelProvider = components["schemas"]["ModelProviderView"];
export type Repo = components["schemas"]["RepoView"];
export type RepoRef = components["schemas"]["RepoRef"];
export type JobRequest = components["schemas"]["JobRequest"];
export type JobKind = JobRequest["kind"];

/**
 * The kinds the form offers, and a compile-time proof that it offers
 * all of them.
 *
 * @decision(DL-061)
 */
export const KINDS = ["pr", "report"] as const satisfies readonly JobKind[];

/**
 * `satisfies` above catches a kind KINDS invents; this catches one it
 * misses. Exported rather than local so it counts as read by
 * `astro check`'s unused-constant hint. `sync` is left out on purpose: a
 * sync job is seeded from a local checkout, which a browser has not got.
 *
 * @decision(DL-100)
 */
export const everyKindIsOffered: Exclude<
  JobKind,
  (typeof KINDS)[number] | "sync"
> extends never
  ? true
  : never = true;

/**
 * What each kind delivers, and where the reader will find it.
 *
 * Both halves matter and the second is the one that is easy to get
 * wrong: a person choosing a kind is deciding what they get, not where
 * spov reads it from. `out/report.md` is spov's own collection path and
 * naming it here answers a question nobody standing at this form is
 * asking.
 */
export const KIND_BLURB: Record<(typeof KINDS)[number], string> = {
  pr: "Pushes a branch and opens a pull request (a merge request on GitLab).",
  report: "Writes a report you read on the session page.",
};

/**
 * repoKey identifies one repository for `{#each}` and for a checkbox.
 * Never parsed back — the selection is held as RepoRefs, not strings.
 *
 * @decision(DL-061)
 */
export function repoKey(r: RepoRef): string {
  return `${r.platform}/${r.owner}/${r.name}`;
}

/** refOf is a listing entry reduced to what a job request carries. */
export function refOf(r: Repo): RepoRef {
  return { platform: r.platform, owner: r.owner, name: r.name };
}

/**
 * repoMatches filters the picker. Every term must appear somewhere in
 * the repository's platform, owner or name.
 *
 * @decision(DL-061)
 */
export function repoMatches(r: Repo, query: string): boolean {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return true;
  const hay = `${r.platform} ${r.owner}/${r.name}`.toLowerCase();
  return terms.every((t) => hay.includes(t));
}

export type PlatformTier = components["schemas"]["PlatformTier"];

/** The tiers a reader is told about, by the API's names. */
function tierNoun(tier: string): string {
  return tier === "org" ? "the organisation's credential" : "your own credential";
}

/**
 * repoUnreachable says why a launch from the browser cannot use a
 * repository, or "" when it can: the listing names the tiers that reach
 * it (`via`), and the platform's tier is the one a browser launch uses.
 * A listing with no tiers (a local server) reaches everything it lists.
 *
 * @decision(DL-042)
 */
export function repoUnreachable(repo: Repo, platforms: PlatformTier[] | null | undefined): string {
  const via = repo.via ?? [];
  if (via.length === 0) return "";
  const platform = (platforms ?? []).find((p) => p.platform === repo.platform);
  const tier = platform?.tier;
  if (!tier || via.includes(tier as (typeof via)[number])) return "";
  // A browser launch has no credential here, though another tier lists it.
  if (tier === "none") return platform?.reason || "No credential a launch from the browser can use reaches this.";
  return `Only ${via.map(tierNoun).join(" and ")} reaches this, and a launch from the browser uses ${tierNoun(tier)}.`;
}

/**
 * repoReadNote is the note on a repository a browser launch would reach
 * through the person's own fine-grained token only because it is public:
 * the token can read it and can push only if it was given it, which
 * GitHub does not say. "" otherwise, including when the browser's launch
 * uses the organisation's credential.
 *
 * @decision(DL-042)
 */
export function repoReadNote(repo: Repo, platforms: PlatformTier[] | null | undefined): string {
  if (!repo.maybe_read_only) return "";
  const tier = (platforms ?? []).find((p) => p.platform === repo.platform)?.tier;
  if (tier !== "user") return "";
  return "Public: your token can read it, and can push only if you gave it this repository.";
}

/** isSelected and toggleRepo hold the selection by value, not by key. */
export function isSelected(selected: RepoRef[], r: RepoRef): boolean {
  return selected.some((s) => repoKey(s) === repoKey(r));
}

export function toggleRepo(selected: RepoRef[], r: RepoRef): RepoRef[] {
  return isSelected(selected, r)
    ? selected.filter((s) => repoKey(s) !== repoKey(r))
    : [...selected, r];
}

/** One provider's models, as the select renders them. */
export interface ModelGroup {
  /** OpenCode's provider id, for a key. */
  provider: string;
  /** The provider's own name, for prose. */
  name: string;
  /** What the optgroup says, including why it cannot be picked. */
  label: string;
  /** Whether a launch by this caller would carry a key for it. */
  available: boolean;
  /** Whether connecting OpenCode in Settings would supply the key. */
  signIn?: boolean;
  /** Full `provider/model` references, as the model field takes them. */
  models: string[];
  /**
   * The references among `models` that models.dev marks beta.
   *
   * @decision(DL-013)
   */
  beta?: string[];
}

/**
 * modelGroups turns GET /api/v1/models into what the select renders. A
 * provider the caller holds no key for is kept and marked, labelled
 * with the variable to set. Available providers come first, catalogue
 * order kept within each half.
 *
 * `providers` and `models` both arrive as `T[] | null`: huma marks a Go
 * slice nullable and spov sets `nullable:"false"` nowhere.
 *
 * @decision(DL-061) @decision(DL-013)
 */
export function modelGroups(
  providers: ModelProvider[] | null | undefined,
): ModelGroup[] {
  const groups: ModelGroup[] = [];
  for (const p of providers ?? []) {
    const models = p.models ?? [];
    // Nothing to pick, so nothing to show, unless a custom name could
    // still be launched on it (DL-013).
    if (models.length === 0 && !p.have_key) continue;
    const signIn = p.sign_in === "opencode";
    groups.push({
      provider: p.provider,
      name: p.name,
      label: p.have_key
        ? p.name
        : signIn
          ? `${p.name} (connect OpenCode or set ${p.key_env})`
          : `${p.name} (needs ${p.key_env})`,
      available: p.have_key,
      signIn,
      models,
      beta: p.beta ?? [],
    });
  }
  return [
    ...groups.filter((g) => g.available),
    ...groups.filter((g) => !g.available),
  ];
}

/** The form's state, as the component holds it. */
export interface JobDraft {
  goal: string;
  kind: JobKind;
  repos: RepoRef[];
  /**
   * A `provider/model` reference. Empty is NOT "OpenCode's default": it
   * is sent as nothing and the server resolves it to the caller's
   * account default, then to OpenCode's (DL-014).
   */
  model: string;
  /** A shell command, run at /workspace; empty leaves gate 1 unverified. */
  check: string;
  /**
   * The wall clock, as a Go duration such as "30m". Empty sends nothing,
   * and the server applies its default (DL-049).
   */
  clock: string;
  /**
   * The git access, "repos" or "all". Empty sends nothing, and the
   * server applies the caller's job default, then repos (DL-043).
   */
  access: "" | GitAccess;
  /**
   * A sandbox template's name. Empty is None, which sends nothing; it is
   * the opening value on every load and is never remembered (DL-121).
   */
  template: string;
}

/** A git access value the API takes. @decision(DL-043) */
export type GitAccess = NonNullable<JobRequest["git_access"]>;

/**
 * defaultAccessLabel is the git access select's first option, which
 * sends nothing: it names what the job default makes of that, and
 * claims nothing before the profile has arrived (as DL-061's model
 * label does).
 *
 * @decision(DL-043) @decision(DL-061)
 */
export function defaultAccessLabel(jobDefault: string | undefined, loaded: boolean): string {
  if (!loaded) return "Your job default";
  return jobDefault === "all"
    ? "Every repository (your job default)"
    : "This job's repositories (your job default)";
}

/**
 * The empty form. `report` rather than `pr` deliberately: both spend a
 * sandbox, but only `pr` pushes a branch and opens a pull request on a
 * real forge, so the higher-blast-radius kind is the one a reader has
 * to choose rather than the one they get by not looking.
 */
export function emptyDraft(): JobDraft {
  return { goal: "", kind: "report", repos: [], model: INHERIT, check: "", clock: "", access: "", template: "" };
}

/**
 * draftProblems says what is stopping a submit, in the reader's own
 * terms and before a request is made. It checks only what the form
 * itself knows — a goal and at least one repository — never the size
 * limits, model validity or quota, which are the server's.
 *
 * @decision(DL-061)
 */
export function draftProblems(draft: JobDraft): string[] {
  const problems: string[] = [];
  if (!draft.goal.trim()) problems.push("a job needs a goal");
  if (draft.repos.length === 0) problems.push("choose at least one repository");
  return problems;
}

/**
 * toJobRequest builds the body POST /api/v1/jobs takes. An empty model,
 * check or git access is omitted rather than sent as "" — Settings does
 * the opposite with the same fields, since there it is the setting. A
 * template is sent only when one is chosen, and a control the person
 * set is still sent, so it wins over the template's.
 *
 * @decision(DL-061) @decision(DL-014) @decision(DL-043) @decision(DL-121)
 */
export function toJobRequest(draft: JobDraft): JobRequest {
  const body: JobRequest = {
    goal: draft.goal.trim(),
    kind: draft.kind,
    repos: draft.repos,
  };
  const model = draft.model.trim();
  if (model) body.model = model;
  const check = draft.check.trim();
  if (check) body.check = check;
  if (draft.clock) body.max_duration = draft.clock;
  if (draft.access) body.git_access = draft.access;
  // @decision(DL-121)
  if (draft.template) body.template = draft.template;
  return body;
}

/**
 * The wall clocks the form offers besides the default. The server's
 * default is one hour, clamped to the caller's limit.
 *
 * @decision(DL-049)
 */
export const WALL_CLOCKS = ["30m", "2h", "3h"] as const;

/** The server's default wall clock, in minutes. */
const DEFAULT_CLOCK_MINUTES = 60;

export { parseDuration };

/**
 * clockOptions keeps the offered clocks the caller's limit allows. An
 * unreadable limit offers them all, and the server refuses one over it.
 *
 * @decision(DL-049)
 */
export function clockOptions(limit: string | undefined): string[] {
  const max = limit ? parseDuration(limit) : null;
  return WALL_CLOCKS.filter((c) => max === null || (parseDuration(c) ?? Infinity) <= max);
}

/**
 * defaultClockLabel names what an empty choice gets: the caller's own
 * default when their profile sets one, else one hour, and the caller's
 * limit when that is lower than either.
 *
 * @decision(DL-049)
 */
export function defaultClockLabel(limit: string | undefined, own?: string): string {
  const max = limit ? parseDuration(limit) : null;
  const mine = own ? parseDuration(own) : null;
  const wanted = mine ?? DEFAULT_CLOCK_MINUTES;
  if (max !== null && max < wanted) return `Your limit (${limitLength(max)})`;
  if (mine !== null) return `Your default (${limitLength(mine)})`;
  return "1 hour (the default)";
}

/**
 * runsFor is how long a job from this form may run, in minutes: the
 * chosen clock, else what defaultClockLabel names. Null when the chosen
 * value cannot be read.
 *
 * @decision(DL-056) @decision(DL-049)
 */
export function runsFor(clock: string, limit: string | undefined, own?: string): number | null {
  if (clock) return parseDuration(clock);
  const max = limit ? parseDuration(limit) : null;
  const wanted = (own ? parseDuration(own) : null) ?? DEFAULT_CLOCK_MINUTES;
  return max !== null && max < wanted ? max : wanted;
}

/** spokenLength says a limit in words: "1 hour", "1 hour 30 minutes". */
export function spokenLength(mins: number): string {
  const whole = Math.round(mins);
  const h = Math.floor(whole / 60);
  const m = whole % 60;
  const unit = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
  if (h === 0) return unit(m, "minute");
  return m === 0 ? unit(h, "hour") : `${unit(h, "hour")} ${unit(m, "minute")}`;
}

/**
 * repoPhrase names the chosen repositories for the submit bar: one or
 * two by name, more by count.
 *
 * @decision(DL-056)
 */
export function repoPhrase(repos: RepoRef[]): string {
  const chosen = repos.map((r) => `${r.owner}/${r.name}`);
  if (chosen.length <= 2) return chosen.join(" and ");
  return `${chosen.length} repositories`;
}

/**
 * sessionPath is where a created job sends the browser: the session,
 * not the job. The id goes in the query string because the app is a
 * static build and internal/webui has no file for /session/<id>.
 *
 * @decision(DL-061) @decision(DL-055)
 */
export function sessionPath(sessionID: string): string {
  return `/session/?id=${encodeURIComponent(sessionID)}`;
}

/**
 * suppressImplicitSubmit stops Enter in a single-line field from
 * submitting the form: HTML implicit submission fires the default
 * button, and this form's button provisions a billable sandbox. The
 * goal textarea is unaffected — Enter there inserts a newline.
 *
 * @decision(DL-061)
 */
export function suppressImplicitSubmit(e: {
  key: string;
  preventDefault: () => void;
}): void {
  if (e.key === "Enter") e.preventDefault();
}

/**
 * Committed is a job the server accepted that the browser could not act
 * on: a 2xx with no session id, or a redirect that threw. Distinct from
 * ApiError because the recovery is the opposite: a caller must not
 * re-enable its submit for this, since retrying buys a second sandbox
 * for a job that already exists.
 *
 * @decision(DL-061)
 */
export class Committed extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Committed";
  }
}

/** What createAndGo needs of the API client, so a test can supply it. */
export type PostJob = (body: JobRequest) => Promise<{
  response: { ok: boolean; status: number };
  data?: { session_id?: string };
  error?: unknown;
}>;

/**
 * createAndGo posts the draft and, on success, leaves for the session's
 * page. The create and the redirect are one function here rather than
 * in the component, so the `nav` seam is one a test can watch.
 *
 * A refusal throws ApiError; a job the server took but the browser
 * could not follow throws Committed.
 *
 * @decision(DL-061)
 */
export async function createAndGo(
  draft: JobDraft,
  post: PostJob,
  go: (url: string) => void,
): Promise<void> {
  const r = await post(toJobRequest(draft));
  if (!r.response.ok)
    throw new ApiError(r.response.status, r.error as Problem | undefined);
  const id = r.data?.session_id;
  if (!id) {
    throw new Committed(
      "The job was created, but the server's answer carried no session to open." +
        " Find it in your sessions rather than starting another.",
    );
  }
  try {
    go(sessionPath(id));
  } catch {
    throw new Committed(
      "The job was created, but this page could not open its session." +
        " Find it in your sessions rather than starting another.",
    );
  }
}

/** names joins a list the way prose does: "a, b and c". */
function names(groups: ModelGroup[]): string {
  const all = groups.map((g) => g.name);
  if (all.length <= 1) return all.join("");
  return `${all.slice(0, -1).join(", ")} and ${all[all.length - 1]}`;
}

/**
 * keyNote says which providers the caller can actually pick from, as
 * visible prose beside the select — a disabled optgroup is commonly
 * skipped by a screen reader. Leads with what is available; the exact
 * variable to set stays on the optgroup rather than being repeated here
 * for every unavailable provider.
 *
 * A provider an OpenCode sign-in would unlock gets a second sentence
 * saying so; `where` is where the sign-in is, from the reader's page.
 *
 * @decision(DL-061) @decision(DL-013) @decision(DL-015)
 */
export function keyNote(groups: ModelGroup[], where = "in Settings"): string {
  const have = groups.filter((g) => g.available);
  const lack = groups.filter((g) => !g.available);
  if (groups.length === 0 || lack.length === 0) return "";
  const first =
    have.length === 0
      ? "No provider has a key, so only OpenCode's own default can run."
      : `${names(have)} ${have.length === 1 ? "has" : "have"} a key; ` +
        `${names(lack)} ${lack.length === 1 ? "does" : "do"} not, ` +
        `so ${lack.length === 1 ? "it is" : "they are"} listed but cannot be chosen.`;
  const unlock = lack.filter((g) => g.signIn);
  if (unlock.length === 0) return first;
  return `${first} Connect OpenCode ${where} to use ${names(unlock)}.`;
}

/**
 * INHERIT is what the model select's first option sends: nothing. `""`
 * on both labels below, since `toJobRequest` omits an empty model and
 * DL-014 resolves an omitted one to the caller's stored default, then
 * to OpenCode's own.
 *
 * @decision(DL-061) @decision(DL-014)
 */
export const INHERIT = "";

/**
 * defaultOptionLabel is the first option's text. `loaded` is a third
 * state, not a nicety: before `/me` answers, both settled labels would
 * be a claim about the account that may be false.
 *
 * @decision(DL-061)
 */
export function defaultOptionLabel(accountDefault: string, loaded: boolean): string {
  if (!loaded) return "Whatever your account resolves to";
  const model = accountDefault.trim();
  return model ? `Your default (${model})` : "OpenCode's default";
}

/**
 * defaultModelNote says where the opening value came from and states
 * that changing the select never writes the account setting back.
 *
 * @decision(DL-061) @decision(DL-062)
 */
export function defaultModelNote(
  accountDefault: string,
  loaded: boolean,
  groups: ModelGroup[],
): string {
  const model = accountDefault.trim();
  if (!loaded || !model) return "";
  // An empty catalogue is "not known yet", never "nothing is launchable".
  if (groups.length > 0 && !canLaunchOn(groups, model)) {
    return (
      `Your account default is ${model}, and this deployment cannot run it, so` +
      " a job that names no model will be refused. Choose one below, or change" +
      " your default in Settings."
    );
  }
  return (
    `Leaving this alone runs the job on ${model}, your account default.` +
    " Choosing another applies to this job only; the default itself is in Settings."
  );
}

/**
 * withoutVariant drops a reference's `#variant` suffix, exactly as the
 * server does — mirrors `prep.LookupModel`. Split on the first `#`; a
 * half that would be empty is not a variant. `settings.ts` re-exports
 * this rather than carrying its own copy.
 *
 * @decision(DL-061) @decision(DL-062) @decision(DL-013)
 */
export function withoutVariant(ref: string): string {
  const hash = ref.indexOf("#");
  if (hash < 0) return ref;
  const model = ref.slice(0, hash);
  const variant = ref.slice(hash + 1);
  if (!model || !variant) return ref;
  return model;
}

/**
 * canLaunchOn reports whether a reference names a model this caller
 * could actually run: under a provider they hold a key for, listed or
 * not (DL-013).
 * The variant is stripped before the comparison — a `#variant` matched
 * literally would report a launchable default as unrunnable.
 *
 * @decision(DL-061) @decision(DL-014) @decision(DL-013)
 */
export function canLaunchOn(groups: ModelGroup[], ref: string): boolean {
  const model = ref.trim();
  const bare = withoutVariant(model);
  // A name the list lacks, under a provider with a key, launches with a
  // note (DL-013).
  const provider = model.split("/")[0];
  return groups.some(
    (g) =>
      g.available &&
      (g.models.includes(model) || g.models.includes(bare) || (g.provider === provider && bare.length > provider.length + 1)),
  );
}
