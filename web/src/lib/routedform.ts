import type { components } from "./api/types";

/**
 * The routed credential form's logic: the draft the form edits, the
 * request it sends and the checks that need no server. The secret is a
 * field of the draft and nothing here keeps a copy of it.
 *
 * @decision(DL-118)
 */

export type Route = components["schemas"]["Route"];
export type RoutedCredential = components["schemas"]["RoutedCredentialView"];
export type RoutedBody = components["schemas"]["PutRoutedCredentialInputBody"];
export type Preset = NonNullable<Route["preset"]>;
export type Scheme = NonNullable<Route["scheme"]>;
export type Attach = NonNullable<Route["attach"]>;

/** Whose credential a write stores. @decision(DL-074) */
export type Owner = "me" | "org";

/** The presets, in the order the command's help names them. @decision(DL-076) */
export const PRESETS = ["npm", "maven", "nuget", "pypi"] as const satisfies readonly Preset[];

/** What the form says of each preset's own options. */
export const PRESET_BLURB: Record<Preset, string> = {
  npm: "An npm registry. The token is sent as a bearer.",
  maven: "A Maven repository, with its username.",
  nuget: "A NuGet feed. Without a username of your own it is spov.",
  pypi: "A Python index. Without a username of your own it is __token__.",
};

/** The form's state. Every list is text, split when the request is built. */
export interface RouteDraft {
  owner: Owner;
  name: string;
  /** A preset's name, or "custom" for hosts and a scheme. */
  kind: "custom" | Preset;
  /** Exact hosts, separated by spaces, commas or new lines. */
  hosts: string;
  /** Path prefixes, separated the same way. */
  paths: string;
  /** Empty until one is chosen: the form never picks a scheme itself. */
  scheme: "" | Scheme;
  /** The Basic username, or a preset's. */
  user: string;
  /** The header's name, for the header scheme. */
  header: string;
  /** A preset's registry address. */
  url: string;
  /** npm's scope, as @acme. */
  scope: string;
  /** Maven's repository id. */
  repoID: string;
  attach: Attach;
  /** A stored preset's widened paths, carried through a replace. */
  widened: string[];
  secret: string;
}

export function emptyRouteDraft(owner: Owner = "me"): RouteDraft {
  return {
    owner,
    name: "",
    kind: "custom",
    hosts: "",
    paths: "",
    scheme: "",
    user: "",
    header: "",
    url: "",
    scope: "",
    repoID: "",
    attach: "always",
    widened: [],
    secret: "",
  };
}

/**
 * draftFromRoute prefills a replace from the listing. The secret is
 * always empty: a PUT replaces the route and the secret together, and
 * no response carries one back.
 *
 * @decision(DL-118) @decision(DL-073)
 */
export function draftFromRoute(route: Route, owner: Owner): RouteDraft {
  const d = emptyRouteDraft(owner);
  d.name = route.name;
  d.attach = route.attach === "asked" ? "asked" : "always";
  d.user = route.user ?? "";
  if (route.preset) {
    d.kind = route.preset;
    d.url = route.url ?? "";
    d.scope = route.scope ?? "";
    d.repoID = route.repo_id ?? "";
    // Kept only when the CLI's --path widened it past the address's own
    // path, so a replace does not narrow the credential unnoticed.
    d.widened = (route.paths ?? []).filter((p) => p !== ownPath(d.url));
    return d;
  }
  d.hosts = (route.hosts ?? []).join("\n");
  d.paths = (route.paths ?? []).join("\n");
  d.scheme = route.scheme ?? "";
  d.header = route.header ?? "";
  return d;
}

/** ownPath is the prefix a preset takes from its address by itself. */
function ownPath(url: string): string {
  try {
    const path = new URL(url).pathname.replace(/\/$/, "");
    return path === "" ? "" : path + "/";
  } catch {
    return "";
  }
}

/** listOf splits a field of hosts or paths. */
export function listOf(text: string): string[] {
  return text.split(/[\s,]+/).filter(Boolean);
}

/**
 * toRoute is the route a draft describes. For a preset it is the
 * preset's name and its own options and nothing else: the hosts, the
 * scheme, the address's own path and a default username are the
 * server's to work out. Paths go only when the stored credential's were
 * widened, and the server checks that they still hold the address.
 *
 * @decision(DL-118) @decision(DL-076)
 */
export function toRoute(draft: RouteDraft): Route {
  const name = draft.name.trim();
  if (draft.kind !== "custom") {
    const route: Route = { name, preset: draft.kind, url: draft.url.trim(), attach: draft.attach };
    if (draft.widened.length > 0) route.paths = draft.widened;
    const user = draft.user.trim();
    if (draft.kind === "npm") {
      if (draft.scope.trim()) route.scope = draft.scope.trim();
      return route;
    }
    if (user) route.user = user;
    if (draft.kind === "maven" && draft.repoID.trim()) route.repo_id = draft.repoID.trim();
    return route;
  }
  const route: Route = { name, hosts: listOf(draft.hosts), attach: draft.attach };
  const paths = listOf(draft.paths);
  if (paths.length > 0) route.paths = paths;
  if (draft.scheme) route.scheme = draft.scheme;
  if (draft.scheme === "basic") route.user = draft.user.trim();
  if (draft.scheme === "header") route.header = draft.header.trim();
  return route;
}

/**
 * toRoutedBody is what the PUT takes. The server trims the secret.
 *
 * @decision(DL-118)
 */
export function toRoutedBody(draft: RouteDraft): RoutedBody {
  return { secret: draft.secret, route: toRoute(draft) };
}

/** A routed credential's name, and a template's. @decision(DL-072) */
const NAME = /^[a-z][a-z0-9-]{0,39}$/;

/** nameProblem says what is wrong with a name's shape, or "". */
export function nameProblem(name: string): string {
  if (!name) return "give it a name";
  if (NAME.test(name)) return "";
  return "the name is lowercase letters, digits and -, starting with a letter, at most 40 characters";
}

/** The names a listing holds. */
export function heldNames(list: RoutedCredential[] | null | undefined): string[] {
  return (list ?? []).map((rc) => rc.route.name);
}

/**
 * routeProblems says what stops a save, before any request: the checks
 * that need no server. `held` is the names the chosen owner's listing
 * holds, asked only of an add, which never replaces.
 *
 * @decision(DL-118)
 */
export function routeProblems(draft: RouteDraft, mode: "add" | "replace", held: string[]): string[] {
  const problems: string[] = [];
  const name = draft.name.trim();
  const shape = nameProblem(name);
  if (shape) problems.push(shape);
  else if (mode === "add" && held.includes(name)) {
    problems.push(
      draft.owner === "org"
        ? `the organisation already has ${name}: use Replace on its row`
        : `you already have ${name}: use Replace on its row`,
    );
  }
  if (draft.kind === "custom") {
    if (listOf(draft.hosts).length === 0) problems.push("name at least one host");
    if (!draft.scheme) problems.push("choose how the secret is sent");
    if (draft.scheme === "basic" && !draft.user.trim()) problems.push("Basic needs a username");
    if (draft.scheme === "header" && !draft.header.trim()) problems.push("name the header");
  } else {
    if (!draft.url.trim()) problems.push("give the registry's address");
    if (draft.kind === "maven" && !draft.user.trim()) problems.push("Maven needs a username");
  }
  if (!draft.secret.trim()) problems.push("type the secret");
  return problems;
}

/**
 * storedNote is the line after a save. Nothing checked the secret, and
 * the line says so, as the command does.
 *
 * @decision(DL-118) @decision(DL-077)
 */
export function storedNote(name: string, owner: Owner): string {
  const whose = owner === "org" ? " for the organisation" : "";
  return `Stored ${name}${whose}, unchecked: spov cannot test it, so a wrong secret shows only when a session uses it.`;
}

/**
 * noEnterSubmit stops Enter in any input of a form from submitting it.
 * It goes on the form, and leaves a textarea's new line and a button's
 * own Enter alone.
 *
 * @decision(DL-118) @decision(DL-061)
 */
export function noEnterSubmit(e: { key: string; target: unknown; preventDefault: () => void }): void {
  if (e.key !== "Enter") return;
  const tag = (e.target as { tagName?: string } | null)?.tagName;
  if (tag === "INPUT" || tag === "SELECT") e.preventDefault();
}
