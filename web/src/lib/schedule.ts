import type { components } from "./api/types";
import { draftProblems, toJobRequest, withoutVariant, type JobDraft, type JobRequest } from "./jobform";
import { modeOf } from "./reasoning";

/**
 * The schedule pages' logic: a cadence chosen by name or written as cron,
 * the words for one, a schedule's request from the job form, and the
 * form's draft from a stored schedule. The server decides what is valid
 * and when a schedule next runs (DL-191); nothing here estimates that.
 *
 * @decision(DL-195)
 */

export type ScheduleView = components["schemas"]["ScheduleView"];
export type ScheduleRequest = components["schemas"]["ScheduleRequest"];
export type SchedulePatch = components["schemas"]["SchedulePatch"];
export type ScheduleFire = components["schemas"]["ScheduleFireView"];

/** How a cadence is chosen in the form. */
export type CadenceKind = "hourly" | "daily" | "weekly" | "monthly" | "custom";

export interface Cadence {
  kind: CadenceKind;
  /** The minute past the hour, 0 to 59. */
  minute: number;
  /** The hour, 0 to 23, for daily, weekly and monthly. */
  hour: number;
  /** The day of the week, 0 (Sunday) to 6, for weekly. */
  weekday: number;
  /** The day of the month, 1 to 28, for monthly. */
  day: number;
  /** The expression as written, for custom. */
  cron: string;
}

export const WEEKDAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

/** A new schedule's cadence: every day at nine. */
export function defaultCadence(): Cadence {
  return { kind: "daily", minute: 0, hour: 9, weekday: 1, day: 1, cron: "" };
}

/** cronOf is the cron expression a cadence sends. */
export function cronOf(c: Cadence): string {
  switch (c.kind) {
    case "hourly":
      return `${c.minute} * * * *`;
    case "daily":
      return `${c.minute} ${c.hour} * * *`;
    case "weekly":
      return `${c.minute} ${c.hour} * * ${c.weekday}`;
    case "monthly":
      return `${c.minute} ${c.hour} ${c.day} * *`;
    default:
      return c.cron.trim();
  }
}

const num = (s: string, lo: number, hi: number): number | null =>
  /^\d{1,2}$/.test(s) && +s >= lo && +s <= hi ? +s : null;

/**
 * cadenceOf reads a stored expression back into the form's choices: one
 * of the named shapes, else custom as written.
 */
export function cadenceOf(cron: string): Cadence {
  const c: Cadence = { ...defaultCadence(), kind: "custom", cron: cron.trim() };
  const desc: Record<string, Partial<Cadence>> = {
    "@hourly": { kind: "hourly", minute: 0 },
    "@daily": { kind: "daily", minute: 0, hour: 0 },
    "@weekly": { kind: "weekly", minute: 0, hour: 0, weekday: 0 },
    "@monthly": { kind: "monthly", minute: 0, hour: 0, day: 1 },
  };
  if (desc[c.cron]) return { ...c, ...desc[c.cron], cron: "" };
  const f = c.cron.split(/\s+/);
  if (f.length !== 5) return c;
  const [mi, h, dom, mon, dow] = f;
  const minute = num(mi, 0, 59);
  if (minute === null || mon !== "*") return c;
  if (h === "*" && dom === "*" && dow === "*") return { ...c, kind: "hourly", minute, cron: "" };
  const hour = num(h, 0, 23);
  if (hour === null) return c;
  if (dom === "*" && dow === "*") return { ...c, kind: "daily", minute, hour, cron: "" };
  const weekday = num(dow, 0, 6);
  if (dom === "*" && weekday !== null) return { ...c, kind: "weekly", minute, hour, weekday, cron: "" };
  const day = num(dom, 1, 28);
  if (dow === "*" && day !== null) return { ...c, kind: "monthly", minute, hour, day, cron: "" };
  return c;
}

const two = (n: number) => String(n).padStart(2, "0");

/**
 * cadenceWords says a cadence in words, its zone after it: "Every day at
 * 09:00 (Europe/Stockholm)"; an expression no named shape reads is
 * shown as written.
 */
export function cadenceWords(cron: string, zone: string): string {
  const c = cadenceOf(cron);
  const at = `${two(c.hour)}:${two(c.minute)}`;
  const where = zone && zone !== "UTC" ? ` (${zone})` : " (UTC)";
  switch (c.kind) {
    case "hourly":
      return `Every hour at :${two(c.minute)}`;
    case "daily":
      return `Every day at ${at}${where}`;
    case "weekly":
      return `Every ${WEEKDAYS[c.weekday]} at ${at}${where}`;
    case "monthly":
      return `On day ${c.day} of every month at ${at}${where}`;
    default:
      return `${cron.trim()}${where}`;
  }
}

/** The browser's IANA zone, else UTC. */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

const NAME = /^[a-z0-9][a-z0-9._-]{0,62}$/;

/**
 * scheduleProblems says what stops a save, before the request: the
 * name's shape, an empty custom cadence and what the job form refuses.
 * The cadence's own rules (at most hourly) are the server's to judge.
 */
export function scheduleProblems(name: string, c: Cadence, draft: JobDraft): string[] {
  const out: string[] = [];
  if (!NAME.test(name.trim())) out.push("a schedule needs a name of lower-case letters, digits, dots, dashes or underscores");
  if (c.kind === "custom" && !c.cron.trim()) out.push("a custom cadence needs a cron expression");
  return out.concat(draftProblems(draft));
}

/** The job request fields the form sets; any other the stored one has is kept. */
const FORM_FIELDS = ["goal", "kind", "repos", "model", "check", "max_duration", "git_access", "template"] as const;

/**
 * scheduleJobOf is the job a schedule runs, from the form over what was
 * stored: a field the form shows is the form's, cleared or set, and one
 * it does not show (set from the CLI) is kept.
 */
export function scheduleJobOf(draft: JobDraft, stored?: JobRequest): JobRequest {
  const base: Record<string, unknown> = { ...(stored ?? {}) };
  for (const k of FORM_FIELDS) delete base[k];
  const job = { ...base, ...toJobRequest(draft) } as JobRequest;
  const fields = job as Record<string, unknown>;
  // A mode on the model and a reasoning field are refused together.
  if (job.model) delete fields.reasoning;
  // A value the person chose here is a choice, not a preference.
  if (job.max_duration !== stored?.max_duration) delete fields.max_duration_preferred;
  if (job.git_access !== stored?.git_access) delete fields.git_access_preferred;
  return job;
}

/** toScheduleRequest is the body a new schedule is created with. */
export function toScheduleRequest(name: string, c: Cadence, zone: string, draft: JobDraft): ScheduleRequest {
  return { name: name.trim(), cron: cronOf(c), zone: zone.trim() || "UTC", job: scheduleJobOf(draft) };
}

/** draftOf is the job form's draft for a stored schedule's job. */
export function draftOf(job: JobRequest): JobDraft {
  const model = job.model ?? "";
  // A mode the CLI sent as `reasoning` is the form's mode when the model
  // names none, so saving keeps it.
  const reasoning = job.reasoning && job.reasoning !== "default" ? job.reasoning : "";
  return {
    goal: job.goal ?? "",
    kind: job.kind === "pr" ? "pr" : "report",
    repos: job.repos ?? [],
    model: withoutVariant(model),
    mode: modeOf(model) || (model ? reasoning : ""),
    check: job.check ?? "",
    clock: job.max_duration ?? "",
    access: job.git_access ?? "",
    template: job.template ?? "",
  };
}

/** The words for a run's outcome. */
export function outcomeWord(f: ScheduleFire): string {
  switch (f.outcome) {
    case "started":
      return f.reason === "already run" ? "Already run" : "Started";
    case "skipped":
      return "Skipped";
    case "failed":
      return "Failed";
    default:
      return f.missed === 1 ? "1 run missed" : `${f.missed ?? 0} runs missed`;
  }
}

/**
 * pausedLine is a paused schedule's line: the server's reason, which may
 * already say it was paused, else "Paused".
 */
export function pausedLine(reason?: string | null): string {
  const r = (reason ?? "").trim();
  if (!r) return "Paused";
  return /^paused\b/i.test(r) ? r[0].toUpperCase() + r.slice(1) : `Paused: ${r}`;
}

/**
 * A job's session page, by the name its session has: "job-" and the
 * job's id in lower case, as the server's jobSessionName makes it.
 */
export function jobPage(jobID: string): string {
  return `/session/?id=job-${encodeURIComponent(jobID.toLowerCase())}`;
}

/** A schedule's own page, by its id, which reaches it for an admin too. */
export function schedulePage(id: string): string {
  return `/schedules/view?ref=${encodeURIComponent(id)}`;
}

/**
 * whenIn is a time on a schedule's clock: "Mon 1 Dec, 09:00" in its zone,
 * as its cadence is read.
 */
export function whenIn(iso: string, zone: string): string {
  try {
    return new Intl.DateTimeFormat("en-GB", {
      timeZone: zone || "UTC",
      weekday: "short",
      day: "numeric",
      month: "short",
      hour: "2-digit",
      minute: "2-digit",
    }).format(new Date(iso));
  } catch {
    return iso;
  }
}

/** editedCadence is what an edit sends of the cadence: only what changed. */
export function editedCadence(stored: { cron: string; zone: string }, c: Cadence, zone: string): { cron?: string; zone?: string } {
  const out: { cron?: string; zone?: string } = {};
  const cron = cronOf(c);
  if (cron !== cronOf(cadenceOf(stored.cron))) out.cron = cron;
  const z = zone.trim() || "UTC";
  if (z !== stored.zone) out.zone = z;
  return out;
}
