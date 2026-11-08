import { ApiError, type Problem } from "./api/client";
import { withoutVariant, type ModelGroup } from "./jobform";
import type { Activity } from "./timeline";

/**
 * The model picker's logic: filtering a long list by typing, the
 * deliberate custom entry under a keyed provider, and switching a
 * running session's model. The groups themselves are `modelGroups` in
 * jobform.ts.
 *
 * @decision(DL-061) @decision(DL-014)
 */

/** MaxCustomID bounds a custom model id, as the server does. */
export const MaxCustomID = 200;

/**
 * filterGroups keeps, per group, the models every term of the query
 * matches, searched in the provider's name and id and the model's
 * reference. A group whose name every term matches keeps all its
 * models. Case does not matter.
 *
 * @decision(DL-061)
 */
export function filterGroups(groups: ModelGroup[], query: string): ModelGroup[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return groups;
  const out: ModelGroup[] = [];
  for (const g of groups) {
    const head = `${g.name} ${g.provider}`.toLowerCase();
    if (terms.every((t) => head.includes(t))) {
      out.push(g);
      continue;
    }
    const models = g.models.filter((ref) => {
      const hay = `${head} ${ref.toLowerCase()}`;
      return terms.every((t) => hay.includes(t));
    });
    if (models.length > 0) out.push({ ...g, models });
  }
  return out;
}

/**
 * customProblem says what is wrong with a model id typed for the custom
 * entry under a provider, or "" when it can be sent. It applies the
 * server's rule (bytes, no spaces, controls, format characters or #);
 * the server checks it again.
 *
 * @decision(DL-061) @decision(DL-013)
 */
export function customProblem(provider: string, id: string): string {
  const v = id.trim();
  if (!v) return "Type the model's id, as the provider names it.";
  if (new TextEncoder().encode(v).length > MaxCustomID) return `A model id is at most ${MaxCustomID} bytes.`;
  // Spaces, controls and format characters (which can make one id look like another).
  // eslint-disable-next-line no-control-regex
  if (/[\s#\u0000-\u001f\u007f-\u009f\u00ad\u200b-\u200f\u202a-\u202e\u2060-\u2064\ufeff]/.test(v)) {
    return "A model id has no spaces, control characters or #.";
  }
  if (v.startsWith("/")) return "Type the id without a slash in front.";
  // OpenRouter's own ids hold a slash; any other provider's starting
  // with its own name is the provider typed twice.
  if (provider !== "openrouter" && v.startsWith(`${provider}/`)) {
    return `Type the id without "${provider}/" in front; it is added for you.`;
  }
  return "";
}

/** customRef is the reference a custom id makes under its provider. */
export function customRef(provider: string, id: string): string {
  return `${provider}/${id.trim()}`;
}

/**
 * isListed reports whether a reference is one of the list's, a
 * `#variant` of one included.
 *
 * @decision(DL-061)
 */
export function isListed(groups: ModelGroup[], ref: string): boolean {
  const bare = withoutVariant(ref.trim());
  return groups.some((g) => g.models.includes(ref.trim()) || g.models.includes(bare));
}

/** CustomNote is said beside a chosen model the list lacks. */
export const CustomNote = "Not in spov's model list: it launches anyway, with a note on the session.";

/**
 * pickNote is what to say beside a chosen model the list lacks: that it
 * launches with a note when its provider has a key, and that every
 * launch refuses it otherwise. Null for a listed model or the default.
 *
 * @decision(DL-061) @decision(DL-013)
 */
export function pickNote(groups: ModelGroup[], value: string): { refused: boolean; text: string } | null {
  const ref = value.trim();
  if (!ref || groups.length === 0 || isListed(groups, ref)) return null;
  const provider = ref.split("/")[0];
  const group = groups.find((g) => g.provider === provider);
  if (!group) return { refused: true, text: "No provider by that name is offered here, so every launch will refuse it." };
  if (!group.available) {
    return { refused: true, text: `${group.name} has no key here, so every launch will refuse it.` };
  }
  return { refused: false, text: CustomNote };
}

/**
 * currentModel is the model a session is on: the newest model the log
 * says was selected, else the one it launched with. A subagent's own
 * selection is not the session's.
 *
 * @decision(DL-014) @decision(DL-111)
 */
export function currentModel(
  activity: Activity[],
  launched: string | undefined,
  children: ReadonlySet<string> = new Set(),
): string {
  for (let i = activity.length - 1; i >= 0; i--) {
    const { model: m, session } = activity[i];
    if (m && !(session && children.has(session))) return m;
  }
  return launched ?? "";
}

/** What switching needs of the API client, so a test can supply it. */
export type PostSwitch = (model: string) => Promise<{
  response: { ok: boolean; status: number };
  error?: unknown;
}>;

/**
 * switchTo asks the server to switch the session's model. Success says
 * the sandbox took it; the log's model.selected frame is what the page
 * then shows.
 *
 * @decision(DL-014)
 */
export async function switchTo(model: string, post: PostSwitch): Promise<void> {
  const r = await post(model);
  if (!r.response.ok) throw new ApiError(r.response.status, r.error as Problem | undefined);
}
