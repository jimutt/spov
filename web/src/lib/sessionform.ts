import { ApiError, type Problem } from "./api/client";
import type { components } from "./api/types";
import { Committed, sessionPath, type RepoRef } from "./jobform";

/**
 * The session form's logic: a repository picker and the account's
 * defaults, nothing else. It shares the picker's code and the launch
 * guards with the job form (jobform.ts).
 *
 * @decision(DL-061)
 */

export type SessionRequest = components["schemas"]["SessionRequest"];

/**
 * sessionProblems says what stops a start: only that no repository is
 * chosen. The rest is the server's to refuse.
 *
 * @decision(DL-061)
 */
export function sessionProblems(repos: RepoRef[]): string[] {
  return repos.length === 0 ? ["choose at least one repository"] : [];
}

/**
 * toSessionRequest is the body POST /api/v1/sessions takes from this
 * form: the repositories and a model only when one was chosen, so the
 * server resolves everything else from the account's interactive
 * defaults and its own.
 *
 * @decision(DL-061)
 */
export function toSessionRequest(repos: RepoRef[], model = ""): SessionRequest {
  const body: SessionRequest = { repos };
  if (model.trim()) body.model = model.trim();
  return body;
}

/** What startAndGo needs of the API client, so a test can supply it. */
export type PostSession = (body: SessionRequest) => Promise<{
  response: { ok: boolean; status: number };
  data?: { id?: string };
  error?: unknown;
}>;

/**
 * startAndGo creates the session and leaves for its page, which shows
 * the launch. A refusal throws ApiError; a session the server took but
 * the browser could not follow throws Committed, which must never
 * re-enable Start (DL-061).
 *
 * @decision(DL-061)
 */
export async function startAndGo(
  repos: RepoRef[],
  post: PostSession,
  go: (url: string) => void,
  model = "",
): Promise<void> {
  const r = await post(toSessionRequest(repos, model));
  if (!r.response.ok) throw new ApiError(r.response.status, r.error as Problem | undefined);
  const id = r.data?.id;
  if (!id) {
    throw new Committed(
      "The session was started, but the server's answer did not say which it is." +
        " Find it in your sessions rather than starting another.",
    );
  }
  try {
    go(sessionPath(id));
  } catch {
    throw new Committed(
      "The session was started, but this page could not open it." +
        " Find it in your sessions rather than starting another.",
    );
  }
}
