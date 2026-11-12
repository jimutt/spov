import { api, unwrap } from "./api/client";
import type { components } from "./api/types";
import { routedCredentialLine } from "./format";
import { nameProblem, type RoutedCredential } from "./routedform";

/**
 * Sandbox templates in the browser: the summary line, the draft the
 * Settings form edits, the request a draft builds and the credential
 * checklist. A setting a draft leaves "not set" is absent from the
 * request, because a template says only what it is for.
 *
 * @decision(DL-121) @decision(DL-119)
 */

export type Template = components["schemas"]["TemplateView"];
export type TemplateSettings = components["schemas"]["TemplateSettingsView"];
export type GitAccess = NonNullable<TemplateSettings["git_access"]>;

/**
 * listTemplates reads the caller's templates. Null is a server that
 * keeps none (501), where no section and no picker is drawn.
 *
 * @decision(DL-121)
 */
export async function listTemplates(signal?: AbortSignal): Promise<Template[] | null> {
  const r = await api.GET("/api/v1/me/templates", { signal });
  if (r.response.status === 501) return null;
  return (await unwrap(r)).templates ?? [];
}

function count(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

function joined(names: string[]): string {
  if (names.length <= 1) return names.join("");
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

/**
 * templateSummary is one line of what a template sets, and nothing for
 * a setting it leaves out: "egress install, 2 credentials, 3 variables,
 * rust and postgres@16". Empty for a template that sets nothing.
 *
 * @decision(DL-121)
 */
export function templateSummary(t: TemplateSettings): string {
  const parts: string[] = [];
  if (t.egress) {
    const extra = t.egress_extra_domains?.length ?? 0;
    parts.push(`egress ${t.egress}` + (extra > 0 ? ` with ${count(extra, "more domain")}` : ""));
  }
  if (t.no_credentials) parts.push("no credentials");
  else if (t.credentials?.length) parts.push(count(t.credentials.length, "credential"));
  const vars = Object.keys(t.env ?? {}).length;
  if (vars > 0) parts.push(count(vars, "variable"));
  if (t.addons?.length) parts.push(joined(t.addons));
  if (t.apt?.length) parts.push(count(t.apt.length, "apt package"));
  if (t.git_access) parts.push(t.git_access === "all" ? "git access to every repository" : "git access to its repositories");
  return parts.join(", ");
}

/**
 * missingLine names the credentials a template lists that the caller
 * does not hold now, or "". A launch from such a template is refused.
 *
 * @decision(DL-121) @decision(DL-120)
 */
export function missingLine(t: Template): string {
  const missing = t.missing_credentials ?? [];
  if (missing.length === 0) return "";
  return `You do not hold ${joined(missing)}, so a launch from this template is refused.`;
}

/** The three states of a template's routed credentials. */
export type CredentialChoice = "default" | "none" | "only";

/** One variable's row in the form. */
export interface EnvRow {
  name: string;
  value: string;
}

/** The form's state. An empty value is "not set" for its control. */
export interface TemplateDraft {
  name: string;
  description: string;
  /** "" is not set; otherwise a preset's name. */
  egress: string;
  /** Extra domains, one per line; read only with a preset that is not open. */
  domains: string;
  credentials: CredentialChoice;
  /** The names ticked under "Only these". */
  picked: string[];
  env: EnvRow[];
  /** One per line, as name or name@version. */
  addons: string;
  apt: string;
  access: "" | GitAccess;
}

/** The egress presets the form offers. */
export const EGRESS = ["open", "install", "agent"] as const;

export function emptyTemplateDraft(): TemplateDraft {
  return {
    name: "",
    description: "",
    egress: "",
    domains: "",
    credentials: "default",
    picked: [],
    env: [],
    addons: "",
    apt: "",
    access: "",
  };
}

/** draftFromTemplate opens a stored template for editing. */
export function draftFromTemplate(t: Template): TemplateDraft {
  const listed = t.credentials ?? [];
  return {
    name: t.name,
    description: t.description ?? "",
    egress: t.egress ?? "",
    domains: (t.egress_extra_domains ?? []).join("\n"),
    credentials: t.no_credentials ? "none" : listed.length > 0 ? "only" : "default",
    picked: [...listed],
    env: Object.entries(t.env ?? {})
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([name, value]) => ({ name, value })),
    addons: (t.addons ?? []).join("\n"),
    apt: (t.apt ?? []).join("\n"),
    access: t.git_access ?? "",
  };
}

/** linesOf splits a one-per-line field, dropping blanks. */
export function linesOf(text: string): string[] {
  return text
    .split(/\n/)
    .map((l) => l.trim())
    .filter(Boolean);
}

/** A variable row someone has typed in; an untouched row is no variable. */
function typedRows(rows: EnvRow[]): EnvRow[] {
  return rows.filter((r) => r.name.trim() !== "" || r.value !== "");
}

/** takesDomains says whether a draft's egress takes extra domains. */
export function takesDomains(egress: string): boolean {
  return egress !== "" && egress !== "open";
}

/**
 * templateProblems says what stops a save before any request. `held` is
 * the names of the templates the listing holds, asked only of an add: a
 * PUT replaces a template whole, so an add of a held name would
 * overwrite it unseen.
 *
 * @decision(DL-121)
 */
export function templateProblems(draft: TemplateDraft, mode: "add" | "edit", held: string[]): string[] {
  const problems: string[] = [];
  const name = draft.name.trim();
  const shape = nameProblem(name);
  if (shape) problems.push(shape);
  else if (name === "none") problems.push("none cannot be a template's name");
  else if (mode === "add" && held.includes(name)) problems.push(`you already have ${name}: use Edit on its row`);
  if (draft.credentials === "only" && draft.picked.length === 0) {
    problems.push("tick at least one credential, or choose None to attach none");
  }
  const rows = typedRows(draft.env);
  if (rows.some((r) => r.name.trim() === "")) problems.push("every variable needs a name");
  const names = rows.map((r) => r.name.trim()).filter(Boolean);
  if (new Set(names).size !== names.length) problems.push("a variable is named twice");
  return problems;
}

/**
 * toTemplateBody is the PUT's body: the whole template, with every
 * "not set" control left out. Null when the draft is not a request at
 * all: "Only these" with nothing ticked, which the server would read
 * as an empty list and refuse.
 *
 * @decision(DL-121) @decision(DL-119)
 */
export function toTemplateBody(draft: TemplateDraft): TemplateSettings | null {
  if (draft.credentials === "only" && draft.picked.length === 0) return null;
  const body: TemplateSettings = {};
  const description = draft.description.trim();
  if (description) body.description = description;
  if (draft.egress) {
    body.egress = draft.egress;
    const domains = linesOf(draft.domains);
    if (takesDomains(draft.egress) && domains.length > 0) body.egress_extra_domains = domains;
  }
  if (draft.credentials === "none") body.no_credentials = true;
  if (draft.credentials === "only") body.credentials = [...draft.picked];
  const rows = typedRows(draft.env);
  if (rows.length > 0) body.env = Object.fromEntries(rows.map((r) => [r.name.trim(), r.value]));
  const addons = linesOf(draft.addons);
  if (addons.length > 0) body.addons = addons;
  const apt = linesOf(draft.apt);
  if (apt.length > 0) body.apt = apt;
  if (draft.access) body.git_access = draft.access;
  return body;
}

/** One row of the "Only these" checklist. */
export interface CredentialRow {
  name: string;
  /** Whose a launch would attach, or "missing" for a ticked name nobody holds. */
  owner: "me" | "org" | "missing";
  /** Its hosts, how it is sent and when it attaches. */
  line: string;
  checked: boolean;
}

function rowLine(rc: RoutedCredential): string {
  const route = rc.route;
  return routedCredentialLine(route) + (route.attach === "asked" ? ", only when a launch asks" : ", every session");
}

/**
 * credentialRows is the checklist: one row per credential name across
 * the caller's own and the organisation's, the caller's own where both
 * hold the name, since that is the one a launch attaches. A ticked name
 * neither holds stays as a row, or an edit would drop it unseen.
 *
 * @decision(DL-121) @decision(DL-075)
 */
export function credentialRows(
  own: RoutedCredential[] | null | undefined,
  org: RoutedCredential[] | null | undefined,
  picked: string[],
): CredentialRow[] {
  const rows = new Map<string, CredentialRow>();
  for (const rc of org ?? []) {
    rows.set(rc.route.name, { name: rc.route.name, owner: "org", line: rowLine(rc), checked: false });
  }
  for (const rc of own ?? []) {
    rows.set(rc.route.name, { name: rc.route.name, owner: "me", line: rowLine(rc), checked: false });
  }
  for (const name of picked) {
    const row = rows.get(name);
    if (row) row.checked = true;
    else rows.set(name, { name, owner: "missing", line: "you do not hold this one now", checked: true });
  }
  return [...rows.values()].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

/** togglePicked ticks or unticks one name. */
export function togglePicked(picked: string[], name: string): string[] {
  return picked.includes(name) ? picked.filter((n) => n !== name) : [...picked, name];
}

/**
 * templateAccessLabel is the job form's unset git access option while a
 * chosen template sets git access: it names the template, not the
 * profile. "" when the template sets none.
 *
 * @decision(DL-121) @decision(DL-120)
 */
export function templateAccessLabel(t: Template | undefined): string {
  if (!t?.git_access) return "";
  return t.git_access === "all"
    ? `Every repository (the ${t.name} template)`
    : `This job's repositories (the ${t.name} template)`;
}
