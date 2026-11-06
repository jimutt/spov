import { ApiError } from "./api/client";
import type { Problem } from "./api/client";
import type { components } from "./api/types";
import { Committed } from "./jobform";

/** How a stopped session would resume, as the server says it. */
export type ResumeView = components["schemas"]["ResumeView"];

/**
 * resumeOffered reports whether the page offers Resume: the server sent
 * a resume choice, which it does only to the owner of a stopped session
 * that can come back.
 *
 * @decision(DL-090)
 */
export function resumeOffered(state: string | undefined, resume: ResumeView | undefined | null): boolean {
  return resume != null && (state === "stopped" || state === "failed");
}

/** untilDay is a snapshot's latest day, for the copy. */
export function untilDay(until: string | undefined, locale?: string): string {
  if (!until) return "";
  const d = new Date(until);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(locale, { month: "short", day: "numeric" });
}

/**
 * resumeQuestion is what the second press answers, worded by what will
 * come back. The transcript wording names what is lost before the
 * reader confirms.
 *
 * @decision(DL-090)
 */
export function resumeQuestion(r: ResumeView, locale?: string): string {
  if (r.mode === "snapshot") {
    const day = untilDay(r.until ?? undefined, locale);
    const kept = day ? ` The snapshot is kept until ${day} at the latest.` : "";
    const wait = r.pending ? " The snapshot is still being taken, so the resume waits for it." : "";
    return (
      "Resume this session in a new sandbox? The workspace comes back as it was when it stopped," +
      " uncommitted changes included." + kept + wait
    );
  }
  return (
    "Resume this session from its transcript? The repositories are cloned fresh and the conversation is" +
    " restored, but uncommitted changes and installed packages are gone."
  );
}

/**
 * resumableLine is the listing's quiet note for a stopped session with a
 * snapshot, and empty for anything else.
 *
 * @decision(DL-090)
 */
export function resumableLine(r: ResumeView | undefined | null, locale?: string): string {
  if (!r || r.mode !== "snapshot") return "";
  const day = untilDay(r.until ?? undefined, locale);
  return day ? `Resumable until ${day}` : "Resumable";
}

/** What `resumed` needs of the API client, so a test can supply it. */
export type PostResume = (body: components["schemas"]["ResumeRequest"]) => Promise<{
  response: { ok: boolean; status: number };
  data?: { state?: string };
  error?: unknown;
}>;

/**
 * resumed asks for the resume, with the message to send once the session
 * runs, if any. A refusal throws ApiError; an answer the browser cannot
 * read throws Committed, since the session may be booting and asking
 * again would be refused or buy nothing.
 *
 * @decision(DL-090) @decision(DL-061)
 */
export async function resumed(
  post: PostResume,
  message?: { text: string; id: string; conversation?: string },
): Promise<void> {
  const r = await post(message ? { message: { ...message } } : {});
  if (!r.response.ok) throw new ApiError(r.response.status, r.error as Problem | undefined);
  if (!r.data?.state) {
    throw new Committed(
      "The session was resumed, but the server's answer could not be read. Reload the page rather than resuming again.",
    );
  }
}

/**
 * followsAResume reports whether the page should reopen its live reads:
 * its stream had ended, the page has since seen the session stopped,
 * and it is starting or running again, whoever resumed it. A row still
 * saying running as the stream ends is stale, not a resume.
 *
 * @decision(DL-090) @decision(DL-018)
 */
export function followsAResume(tailEnded: boolean, sawStopped: boolean, state: string | undefined): boolean {
  return tailEnded && sawStopped && (state === "starting" || state === "running");
}

/**
 * watchesForAResume reports whether a stopped session's row is read
 * again on a slow timer, so a page left open sees a resume made
 * elsewhere.
 *
 * @decision(DL-090)
 */
export function watchesForAResume(state: string | undefined, resume: ResumeView | undefined | null): boolean {
  return resumeOffered(state, resume);
}

/** How often a resumable stopped session is read again, in ms. */
export const ResumeWatchEvery = 30_000;
