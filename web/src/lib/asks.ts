import type { components } from "./api/types";
import type { Activity } from "./timeline";
import { ApiError, type Problem } from "./api/client";
import { TurnEnders } from "./steer";

/**
 * The agent's asks: a permission request or a question form, drawn as a
 * card where it was asked and answered through the server. Everything
 * here is read from the stored log (DL-020) and has no component in it,
 * so vitest reaches it.
 *
 * @decision(DL-020)
 */

export type Ask = components["schemas"]["AskEvent"];
export type FormField = components["schemas"]["FormField"];

/** The two frames that open an ask; the other three settle one. */
export const AskOpened = new Set<string>(["permission.asked", "form.created"]);
const PermissionReplied = "permission.replied";
const FormReplied = "form.replied";
const FormCancelled = "form.cancelled";

/**
 * Where an ask stands. `unanswered` is one still pending on a session
 * that has ended. `allowed-always` is an answer given from an attached
 * TUI; the page never sends it (DL-020).
 *
 * @decision(DL-020)
 */
export type AskStatus = "pending" | "allowed" | "allowed-always" | "rejected" | "answered" | "dismissed" | "unanswered";

export interface AskState {
  status: AskStatus;
  /** A form's answer, from its form.replied. */
  answer?: Record<string, unknown>;
}

/** isAskFrame reports one of the sandbox's five ask frames. */
export function isAskFrame(ev: Activity): boolean {
  return ev.source === "ephemeral" && ev.ask !== undefined && ev.ask !== null;
}

/**
 * askStates reads every ask's status out of the log: pending from its
 * asked or created frame, settled by its reply or cancellation, and
 * `unanswered` when `ended` finds it still pending or a turn of its
 * conversation ended after it (an interrupt drops a pending permission
 * with no reply, spike S-F1). A settling frame with no ask before it
 * names nothing the page drew.
 *
 * @decision(DL-020)
 */
export function askStates(activity: Activity[], ended: boolean): Map<string, AskState> {
  const out = new Map<string, AskState>();
  // Pending asks by the conversation they were asked in.
  const asked = new Map<string, string>();
  for (const ev of activity) {
    if (ev.source === "opencode" && TurnEnders.includes(ev.name)) {
      for (const [id, conv] of asked) {
        if (conv !== (ev.session ?? "") || out.get(id)?.status !== "pending") continue;
        out.set(id, { status: "unanswered" });
        asked.delete(id);
      }
      continue;
    }
    if (!isAskFrame(ev)) continue;
    const ask = ev.ask!;
    if (AskOpened.has(ev.name)) {
      if (!out.has(ask.id)) {
        out.set(ask.id, { status: "pending" });
        asked.set(ask.id, ev.session ?? "");
      }
      continue;
    }
    if (out.get(ask.id)?.status !== "pending") continue;
    switch (ev.name) {
      case PermissionReplied:
        out.set(ask.id, {
          status: ask.reply === "reject" ? "rejected" : ask.reply === "always" ? "allowed-always" : "allowed",
        });
        break;
      case FormReplied:
        out.set(ask.id, { status: "answered", answer: (ask.answer ?? {}) as Record<string, unknown> });
        break;
      case FormCancelled:
        out.set(ask.id, { status: "dismissed" });
        break;
    }
  }
  if (ended) {
    for (const [id, s] of out) if (s.status === "pending") out.set(id, { status: "unanswered" });
  }
  return out;
}

/**
 * pendingAsks counts the asks waiting on the reader, for the status bar.
 *
 * @decision(DL-020)
 */
export function pendingAsks(states: Map<string, AskState>): number {
  let n = 0;
  for (const s of states.values()) if (s.status === "pending") n++;
  return n;
}

/**
 * askLine is the one line an answered ask collapses to.
 *
 * @decision(DL-020)
 */
export function askLine(ask: Ask, state: AskState): string {
  switch (state.status) {
    case "allowed":
      return "Allowed once";
    case "allowed-always":
      return "Allowed for good, from an attached TUI";
    case "rejected":
      return "Rejected";
    case "dismissed":
      return "Dismissed";
    case "unanswered":
      return "Not answered";
    case "answered": {
      const said = answerSummary(ask.fields ?? [], state.answer ?? {});
      return said ? `Answered: ${said}` : "Answered";
    }
    default:
      return ask.kind === "permission" ? "Waiting for you to allow or reject" : "Waiting for your answer";
  }
}

/**
 * answerSummary lists an answer's values in field order: "Blue, No".
 *
 * @decision(DL-020)
 */
export function answerSummary(fields: FormField[], answer: Record<string, unknown>): string {
  const keys = fields.map((f) => f.key);
  for (const k of Object.keys(answer)) if (!keys.includes(k)) keys.push(k);
  const parts: string[] = [];
  for (const k of keys) {
    const v = answer[k];
    if (v === undefined || v === null || v === "") continue;
    if (Array.isArray(v)) {
      if (v.length > 0) parts.push(v.map(String).join(", "));
    } else if (typeof v === "boolean") {
      parts.push(v ? "Yes" : "No");
    } else {
      parts.push(String(v));
    }
  }
  return parts.join(", ");
}

/**
 * permissionSummary is what a permission asks, in the card's mono line:
 * the action and its resources.
 *
 * @decision(DL-020)
 */
export function permissionSummary(ask: Ask): string {
  const resources = (ask.resources ?? []).filter((r) => r.trim() !== "");
  return resources.length ? `${ask.action ?? "?"} ${resources.join(" ")}` : (ask.action ?? "?");
}

/**
 * What the reader has entered for one field. `picks` holds chosen option
 * values, with `Other` standing for the free text in `other`; `text` is
 * a free string or a number as typed; `flag` a boolean, null while unset.
 *
 * @decision(DL-020)
 */
export interface FieldDraft {
  picks: string[];
  other: string;
  text: string;
  flag: boolean | null;
}

export type Draft = Record<string, FieldDraft>;

/** Other is the pick that stands for a field's free text. */
export const Other = "\u0000other";

/** The field types this page draws. Another is shown as unsupported. */
export const KnownTypes = new Set(["string", "multiselect", "number", "integer", "boolean", "external"]);

/**
 * emptyDraft is a form's draft before the reader touches it, from each
 * field's default.
 *
 * @decision(DL-020)
 */
export function emptyDraft(fields: FormField[]): Draft {
  // No prototype: a field's key is the sandbox's to choose.
  const out: Draft = Object.create(null);
  for (const f of fields) {
    const d = f.default;
    const draft: FieldDraft = { picks: [], other: "", text: "", flag: null };
    if (typeof d === "string") {
      if ((f.options ?? []).some((o) => o.value === d)) draft.picks = [d];
      else draft.text = d;
    } else if (Array.isArray(d)) {
      draft.picks = d.filter((v): v is string => typeof v === "string");
    } else if (typeof d === "number") {
      draft.text = String(d);
    } else if (typeof d === "boolean") {
      draft.flag = d;
    }
    out[f.key] = draft;
  }
  return out;
}

/**
 * fieldValue is one field's answer from its draft, undefined while it
 * has none or it cannot be read (a number that is not one).
 *
 * @decision(DL-020)
 */
export function fieldValue(f: FormField, d: FieldDraft | undefined): unknown {
  if (!d) return undefined;
  const other = d.other.trim();
  switch (f.type) {
    case "string": {
      if ((f.options ?? []).length === 0) return d.text.trim() || undefined;
      const pick = d.picks[0];
      if (pick === Other) return other || undefined;
      return pick || undefined;
    }
    case "multiselect": {
      const picks = d.picks.filter((p) => p !== Other);
      if (d.picks.includes(Other) && other) picks.push(other);
      return picks.length ? picks : undefined;
    }
    case "number":
    case "integer": {
      if (!d.text.trim()) return undefined;
      const n = Number(d.text);
      if (!Number.isFinite(n) || (f.type === "integer" && !Number.isInteger(n))) return undefined;
      return n;
    }
    case "boolean":
      return d.flag ?? undefined;
  }
  return undefined;
}

/**
 * shownFields is the fields to draw: not hidden, and every `when`
 * condition met by the answer so far. `eq` against a list means the
 * list holds the value.
 *
 * @decision(DL-020)
 */
export function shownFields(fields: FormField[], draft: Draft): FormField[] {
  const byKey = new Map(fields.map((f) => [f.key, f]));
  return fields.filter((f) => {
    if (f.hidden) return false;
    return (f.when ?? []).every((w) => {
      const other = byKey.get(w.key);
      const v = other ? fieldValue(other, draft[w.key]) : undefined;
      const same = Array.isArray(v) ? v.includes(w.value as string) : v === w.value;
      return w.op === "neq" ? !same : same;
    });
  });
}

/**
 * toAnswer builds the answer the server takes, keyed by field key, from
 * the fields shown, with what still stops it from being sent. External
 * fields and types this page does not know send nothing.
 *
 * @decision(DL-020)
 */
export function toAnswer(fields: FormField[], draft: Draft): { answer: Record<string, unknown>; problems: string[] } {
  const answer: Record<string, unknown> = Object.create(null);
  const problems: string[] = [];
  for (const f of shownFields(fields, draft)) {
    if (f.type === "external" || !KnownTypes.has(f.type) || Object.hasOwn(answer, f.key)) continue;
    const name = f.title || f.description || f.key;
    const d = draft[f.key];
    const v = fieldValue(f, d);
    if (v === undefined) {
      if ((f.type === "number" || f.type === "integer") && d?.text.trim()) {
        problems.push(`“${name}” needs ${f.type === "integer" ? "a whole number" : "a number"}.`);
      } else if (d?.picks.includes(Other) && !d.other.trim()) {
        problems.push(`Say what “Other” is for “${name}”.`);
      } else if (f.required) {
        problems.push(`“${name}” needs an answer.`);
      }
      continue;
    }
    answer[f.key] = v;
  }
  if (problems.length === 0 && Object.keys(answer).length === 0) problems.push("Answer at least one question.");
  return { answer, problems };
}

/**
 * webLink is a field's link when it is an absolute http or https
 * address, else null and the page draws it as text. The address is the
 * sandbox's, and an href is where a `javascript:` one would run.
 *
 * @decision(DL-020)
 */
export function webLink(raw: string | undefined): string | null {
  try {
    const u = new URL(raw ?? "");
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** What answering needs of the API client, so a test can supply it. */
export type PostAnswer = () => Promise<{ response: { ok: boolean; status: number }; error?: unknown }>;

/**
 * answer sends one answer (a permission reply, a form's answer or its
 * dismissal). Success says only that the sandbox took it; the outcome is
 * the stored frame `askStates` reads.
 *
 * @decision(DL-020)
 */
export async function answer(post: PostAnswer): Promise<void> {
  const r = await post();
  if (!r.response.ok) throw new ApiError(r.response.status, r.error as Problem | undefined);
}
