import createClient from "openapi-fetch";
import type { paths } from "./types";

/**
 * baseURL is the origin the app was served from. The server serves the
 * built app on every path it does not own itself, so that origin is
 * always the API's own — resolving it here rather than passing a bare
 * "/" keeps the client usable where a relative request URL has no
 * document to resolve against, which is every test and every tool.
 *
 * @decision(DL-005) @decision(DL-055)
 */
export function baseURL(): string {
  return typeof location === "undefined" ? "http://localhost/" : `${location.origin}/`;
}

/**
 * The typed API client. Every operation, its parameters and both its
 * success and `application/problem+json` bodies come from `types.ts`,
 * which is generated from the committed OpenAPI document.
 *
 * Same origin, so the browser's session cookie is the whole of auth:
 * there is no token to plumb, and `credentials` is the one option that
 * decides whether a request is authenticated at all.
 *
 * @decision(DL-005)
 */
export const api = createClient<paths>({
  baseUrl: baseURL(),
  credentials: "same-origin",
});

/** An RFC 9457 problem, as every non-2xx response carries it. */
export type Problem = NonNullable<
  paths["/api/v1/me"]["get"]["responses"][401]["content"]["application/problem+json"]
>;

/** LoginPath is DL-007's browser login flow. */
export const LoginPath = "/login";

/**
 * loginURL is where an unauthenticated page sends the viewer, carrying
 * where they were so the flow can return them to it. `next` is a path
 * only, never an absolute URL.
 *
 * @decision(DL-055) @decision(DL-007)
 */
export function loginURL(here: { pathname: string; search: string }): string {
  const next = here.pathname + here.search;
  return `${LoginPath}?next=${encodeURIComponent(next)}`;
}

/**
 * atLogin reports whether we are already in the login flow, where a
 * redirect to it would be a loop rather than a recovery.
 */
function atLogin(pathname: string): boolean {
  return pathname === LoginPath || pathname.startsWith(`${LoginPath}/`);
}

/**
 * redirectToLogin sends the browser to the login flow, remembering where
 * it was. It is the whole of the app's 401 handling: the `api`
 * middleware below calls it, and so does the SSE path in `live.ts`,
 * which does not go through `openapi-fetch` and would otherwise need a
 * funnel of its own.
 *
 * It is a no-op where there is no document to navigate — a test, or a
 * build-time render — and where we are already in the login flow, which
 * would be a loop rather than a recovery.
 *
 * @decision(DL-055)
 */
export function redirectToLogin(): void {
  if (typeof location === "undefined") return;
  if (atLogin(location.pathname)) return;
  nav.go(loginURL(location));
}

/**
 * nav is how the app leaves the page, a seam so a test can watch the
 * redirect fire.
 *
 * @decision(DL-055)
 */
export const nav = {
  go(url: string): void {
    location.assign(url);
  },
};

/**
 * The one place a 401 is handled for ordinary requests. Every page calls
 * the API through `api`, so an expired session sends the viewer to the
 * login flow from wherever they were, and no page carries auth handling
 * of its own.
 *
 * The landing page is the exception and does not use this client: it has
 * to be able to *render* a signed-out state, and a redirect would make
 * that state unreachable. It uses `probe` below.
 *
 * @decision(DL-055)
 */
api.use({
  onResponse({ response }) {
    if (response.status === 401) redirectToLogin();
    return;
  },
});

/**
 * probe is the same client without the 401 funnel, for the one caller
 * that treats "not signed in" as an answer rather than an interruption.
 *
 * @decision(DL-055)
 */
export const probe = createClient<paths>({
  baseUrl: baseURL(),
  credentials: "same-origin",
});

/**
 * ApiError is a response the caller cannot use: the server refused, or
 * answered something that is not the success body. It carries the RFC
 * 9457 problem where there was one, so a page can render the server's
 * own words rather than inventing a message for them.
 *
 * @decision(DL-055) @decision(DL-005)
 */
export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | undefined;

  constructor(status: number, problem?: Problem) {
    super(problem?.title ?? `the server answered ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

/**
 * unwrap turns one `openapi-fetch` result into its body or an ApiError,
 * so a page reads as a sequence of awaits rather than a chain of
 * `if (error)`.
 *
 * A 204 has no body, and `data` is legitimately undefined there; the
 * status is what says the call succeeded, so it is what this checks.
 *
 * @decision(DL-055)
 */
export async function unwrap<T>(r: {
  data?: T;
  error?: unknown;
  response: Response;
}): Promise<T> {
  if (r.response.ok) return r.data as T;
  throw new ApiError(r.response.status, r.error as Problem | undefined);
}
