import { deliveredSummary, pullRequests } from "./deliverables";
import { live, sentence, type Job, type Session } from "./format";
import type { JobState } from "./steer";

/** The filters on the jobs page. `all` is the default. */
export type Show = "all" | "active" | "needs-you" | "finished";

/**
 * GROUP places every job state in one filter. A Record over JobState, so
 * a state the API gains fails `astro check` until it is placed.
 *
 * @decision(DL-060)
 */
export const GROUP: Record<JobState, Exclude<Show, "all">> = {
  queued: "active",
  provisioning: "active",
  running: "active",
  delivering: "active",
  blocked: "needs-you",
  steered: "needs-you",
  succeeded: "finished",
  failed: "finished",
  cancelled: "finished",
};

/** The filters in the order the page draws them. @decision(DL-060) */
export const FILTERS: readonly { show: Show; label: string }[] = [
  { show: "all", label: "All" },
  { show: "active", label: "Active" },
  { show: "needs-you", label: "Needs you" },
  { show: "finished", label: "Finished" },
];

/**
 * showFrom reads the filter from a query string. Anything it does not
 * know is `all`, so a stale link still shows something.
 *
 * @decision(DL-060)
 */
export function showFrom(search: string): Show {
  const v = new URLSearchParams(search).get("show");
  return FILTERS.some((f) => f.show === v) ? (v as Show) : "all";
}

/**
 * searchFor is the query string that names a filter. `all` has none, so
 * the plain page URL is the default view.
 *
 * @decision(DL-060)
 */
export function searchFor(show: Show): string {
  return show === "all" ? "" : `?show=${show}`;
}

/**
 * statesFor is the listing's `state` parameter for a filter: one
 * comma-separated value, since huma keeps only the first of a repeated
 * one. Undefined for `all`, which sends none.
 *
 * @decision(DL-060) @decision(DL-057)
 */
export function statesFor(show: Show): string | undefined {
  if (show === "all") return undefined;
  return (Object.keys(GROUP) as JobState[])
    .filter((st) => GROUP[st] === show)
    .join(",");
}

/**
 * goalLine is the first non-blank line of a goal, which is what a row
 * shows of it.
 *
 * @decision(DL-060)
 */
export function goalLine(goal: string): string {
  return (
    goal
      .split("\n")
      .map((l) => l.trim())
      .find((l) => l !== "") ?? ""
  );
}

/** One row's outcome line: a link, a note, or nothing. */
export type Outcome =
  | { kind: "link"; href: string; text: string }
  | { kind: "note"; text: string; tone: "soft" | "fail" }
  | { kind: "none" };

/**
 * outcomeOf says how a job came out, or what it is waiting on. One
 * verified pull request is its link; several, or reports, are counted.
 * The listing carries no report's text. Chosen by kind and status, never
 * by the text.
 *
 * @decision(DL-060) @decision(DL-051)
 */
export function outcomeOf(j: Job): Outcome {
  const verified = pullRequests(j).filter((d) => d.status === "verified" && /^https?:\/\//.test(d.url ?? ""));
  if (verified.length === 1 && !j.partial) {
    const url = verified[0].url as string;
    return { kind: "link", href: url, text: url };
  }
  const delivered = deliveredSummary(j);
  if (delivered && j.state === "succeeded") {
    return { kind: "note", text: sentence(delivered), tone: "soft" };
  }
  if (
    j.error &&
    ["blocked", "steered", "failed", "cancelled"].includes(j.state)
  ) {
    return {
      kind: "note",
      text: j.error,
      tone: j.state === "failed" ? "fail" : "soft",
    };
  }
  return { kind: "none" };
}

/**
 * jobAge is how long a job has run, in seconds: to now while it is
 * unfinished and to when it stopped once it has, as `age` is for a
 * session.
 *
 * @decision(DL-060)
 */
export function jobAge(j: Job, now: Date): number {
  const start = Date.parse(j.created_at);
  const end = j.stopped_at ? Date.parse(j.stopped_at) : now.getTime();
  if (Number.isNaN(start) || Number.isNaN(end)) return 0;
  return Math.max(0, Math.floor((end - start) / 1000));
}

/**
 * UNFINISHED is every job state that counts against the job limit: all
 * but the finished ones, queued included, as `store.JobState.Counts`
 * decides on the server.
 *
 * @decision(DL-060) @decision(DL-045)
 */
export const UNFINISHED: readonly JobState[] = (Object.keys(GROUP) as JobState[]).filter(
  (st) => GROUP[st] !== "finished",
);

/** unfinished reports whether a job state counts against the job limit. */
export function unfinished(state: string | undefined): boolean {
  return (UNFINISHED as readonly string[]).includes(state ?? "");
}

/**
 * limitUse counts what the two concurrency limits count, from the
 * session list: jobs that are not finished, and live sessions that do
 * not belong to one. A job's session counts as its job, never as a
 * session, as the server's quota counts it.
 *
 * @decision(DL-060) @decision(DL-045) @decision(DL-009)
 */
export function limitUse(sessions: Session[]): { sessions: number; jobs: number } {
  let s = 0;
  let j = 0;
  for (const x of sessions) {
    if (x.job && unfinished(x.job.state)) j++;
    else if (live(x)) s++;
  }
  return { sessions: s, jobs: j };
}
