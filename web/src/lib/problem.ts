import { ApiError } from "./api/client";
import { StreamProblem } from "./api/sse";

/**
 * Described is what the page shows of a failure: the server's own title
 * and detail where it sent a problem, else the error's message.
 */
export interface Described {
  title: string;
  detail?: string;
  status?: number;
}

/**
 * describeProblem reads a failure the way Problem.svelte draws it. A
 * refused stream carries the server's problem as a request does, so a
 * stream refused by a limit (a 429 saying how many live views one user
 * may hold) shows the server's words rather than only its title.
 *
 * @decision(DL-055) @decision(DL-133)
 */
export function describeProblem(error: unknown): Described {
  const refused = error instanceof ApiError || error instanceof StreamProblem ? error : undefined;
  const message = error instanceof Error ? error.message : String(error ?? "");
  return {
    title: refused?.problem?.title ?? message,
    detail: refused?.problem?.detail,
    status: refused?.status,
  };
}
