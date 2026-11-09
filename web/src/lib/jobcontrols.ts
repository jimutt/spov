import { ApiError } from "./api/client";
import type { Problem } from "./api/client";
import type { JobState } from "./steer";

/**
 * CANCELLABLE says, for every job state, whether the session page offers
 * Cancel job. A Record over JobState, so a state the API gains fails
 * `astro check` until it is decided here. Narrower than the server's
 * `CancelJob`, which also accepts `delivering`.
 *
 * @decision(DL-057)
 */
export const CANCELLABLE: Record<JobState, boolean> = {
  queued: true,
  provisioning: true,
  running: true,
  blocked: true,
  steered: true,
  delivering: false,
  succeeded: false,
  failed: false,
  cancelled: false,
};

/**
 * cancellable reports whether Cancel job is offered. No job, or a state
 * this build does not know, offers nothing: it is a destructive act.
 *
 * @decision(DL-057)
 */
export function cancellable(state: string | undefined): boolean {
  return state !== undefined && Object.hasOwn(CANCELLABLE, state)
    ? CANCELLABLE[state as JobState]
    : false;
}

/**
 * endable reports whether End session is offered: a live session that no
 * unfinished job drives. `jobKnown` is false until the jobs read has
 * answered, and until then a job's session cannot be told from any other,
 * so nothing is offered.
 *
 * @decision(DL-057)
 */
export function endable(
  sessionState: string | undefined,
  jobKnown: boolean,
  jobState: string | undefined,
  held = false,
): boolean {
  if (sessionState !== "starting" && sessionState !== "running") return false;
  if (!jobKnown) return false;
  // A held sync job's sandbox is ended by Discard, never End session.
  // @decision(DL-101)
  if (held) return false;
  return (
    jobState === undefined ||
    ["succeeded", "failed", "cancelled"].includes(jobState)
  );
}

/**
 * discardable reports whether Discard held work is offered: a sync job
 * whose sandbox is held for its state to be taken.
 *
 * @decision(DL-101) @decision(DL-102)
 */
export function discardable(job: { kind: string; sync?: { held: boolean } } | undefined): boolean {
  return job?.kind === "sync" && job.sync?.held === true;
}

/** The question Discard held work's second press answers. @decision(DL-101) */
export const DiscardQuestion =
  "Discard this job's held work? Its sandbox is terminated, and anything not taken with" +
  " spov jobs apply is lost.";

/** The questions the second press answers. @decision(DL-057) */
export const CancelQuestion =
  "Cancel this job? Its sandbox is terminated and the job ends without a verdict." +
  " Work that was not pushed is lost.";
export const EndQuestion =
  "End this session? Its sandbox is terminated, and anything not pushed is lost.";

/** What `ended` needs of the API client, so a test can supply it. */
export type Delete = () => Promise<{
  response: { ok: boolean; status: number };
  error?: unknown;
}>;

/**
 * ended sends a cancel or an end, and throws the server's refusal. Takes
 * `{ok, status}` rather than a `Response`, as `stop` in steer.ts does, so
 * a test can supply one.
 *
 * @decision(DL-057)
 */
export async function ended(del: Delete): Promise<void> {
  const r = await del();
  if (!r.response.ok) {
    throw new ApiError(r.response.status, r.error as Problem | undefined);
  }
}
