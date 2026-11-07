import { describe, expect, it } from "vitest";
import type { Job, Session } from "./format";
import {
  FILTERS,
  GROUP,
  goalLine,
  jobAge,
  linkLabel,
  outcomeOf,
  searchFor,
  showFrom,
  statesFor,
  UNFINISHED,
  limitUse,
  unfinished,
} from "./joblist";
import { KNOWN_JOB_STATES } from "./steer";

function job(over: Partial<Job>): Job {
  return {
    id: "j1",
    session_id: "ses_1",
    owner_id: "u",
    kind: "pr",
    goal: "g",
    state: "running",
    attempt: 1,
    created_at: "2026-09-26T10:00:00Z",
    updated_at: "2026-09-26T10:00:00Z",
    ...over,
  } as Job;
}

describe("the jobs page's filters", () => {
  // @decision(DL-060)
  it("places every state in exactly one filter", () => {
    expect(Object.keys(GROUP).sort()).toEqual([...KNOWN_JOB_STATES].sort());
    const sent = ["active", "needs-you", "finished"].flatMap((f) =>
      (statesFor(f as never) ?? "").split(","),
    );
    expect(sent.sort()).toEqual([...KNOWN_JOB_STATES].sort());
  });

  // @decision(DL-060)
  it("asks for the states of a filter as one parameter, and for none on All", () => {
    expect(statesFor("needs-you")).toBe("blocked,steered");
    expect(statesFor("finished")).toBe("succeeded,failed,cancelled");
    expect(statesFor("all")).toBeUndefined();
  });

  // @decision(DL-060)
  it("reads the filter from the URL and writes it back", () => {
    for (const { show } of FILTERS) {
      expect(showFrom(searchFor(show))).toBe(show);
    }
    expect(searchFor("all")).toBe("");
    expect(showFrom("?show=nonsense")).toBe("all");
    expect(showFrom("")).toBe("all");
  });
});

describe("a link's short label", () => {
  // @decision(DL-106) @decision(DL-051)
  it("names the repository and the change as its forge writes them", () => {
    const url = "https://github.com/acme/app/pull/7";
    expect(linkLabel({ repository: "acme/app", number: 7, forge: "github" }, url)).toBe("acme/app #7");
    expect(linkLabel({ repository: "grp/sub/app", number: 5, forge: "gitlab" }, "https://gitlab.com/x")).toBe(
      "grp/sub/app !5",
    );
  });

  it("falls back to the address without its scheme when the deliverable does not say", () => {
    expect(linkLabel({ repository: "acme/app" }, "https://www.forge.example/acme/app/pulls/7")).toBe(
      "forge.example/acme/app/pulls/7",
    );
    expect(linkLabel({}, "http://forge.example/x")).toBe("forge.example/x");
  });

  it("is what a job's linked outcome carries beside the whole address", () => {
    const url = "https://github.com/acme/app/pull/7";
    const j = job({
      kind: "pr",
      state: "succeeded",
      deliverables: [{ kind: "pr", status: "verified", url, repository: "acme/app", number: 7, forge: "github" }],
    });
    expect(outcomeOf(j)).toEqual({ kind: "link", href: url, text: url, short: "acme/app #7" });
  });
});

describe("a row", () => {
  // @decision(DL-060)
  it("shows the first line of the goal that has anything on it", () => {
    expect(goalLine("\n  \n  Fix the flaky test\nin internal/cli")).toBe(
      "Fix the flaky test",
    );
    expect(goalLine("   ")).toBe("");
  });

  // @decision(DL-060) @decision(DL-051)
  it("links one verified pull request and counts the rest", () => {
    const url = "https://forge.example/acme/app/pulls/7";
    const pr = (status: "verified" | "missing", u?: string) => ({
      kind: "pr" as const,
      status,
      url: u,
      repository: "acme/app",
      branch: "b",
    });
    expect(
      outcomeOf(job({ kind: "pr", state: "succeeded", deliverables: [pr("verified", url)] })),
    ).toEqual({ kind: "link", href: url, text: url, short: url.replace("https://", "") });
    expect(
      outcomeOf(
        job({ kind: "pr", state: "succeeded", deliverables: [pr("verified", url), pr("verified", url)] }),
      ),
    ).toEqual({ kind: "note", text: "2 pull requests", tone: "soft" });
    expect(
      outcomeOf(
        job({
          kind: "pr",
          state: "succeeded",
          partial: true,
          deliverables: [pr("verified", url), pr("missing")],
        }),
      ),
    ).toEqual({ kind: "note", text: "1 pull request, partly done", tone: "soft" });
    // A report is counted, and never linked, whatever its fields say.
    expect(
      outcomeOf(
        job({
          kind: "report",
          state: "succeeded",
          deliverables: [{ kind: "report", status: "verified", path: "a.md", url }],
        }),
      ),
    ).toEqual({ kind: "note", text: "1 report", tone: "soft" });
    expect(
      outcomeOf(job({ kind: "pr", state: "succeeded", deliverables: [pr("verified", "javascript:alert(1)")] })),
    ).toEqual({ kind: "note", text: "1 pull request", tone: "soft" });
  });

  // @decision(DL-060)
  it("says what a job is waiting on or why it failed", () => {
    expect(outcomeOf(job({ state: "blocked", error: "check failed" }))).toEqual(
      {
        kind: "note",
        text: "check failed",
        tone: "soft",
      },
    );
    expect(outcomeOf(job({ state: "failed", error: "gate 3" }))).toMatchObject({
      tone: "fail",
    });
    // A running job's error, if it has one, is not an outcome yet.
    expect(outcomeOf(job({ state: "running", error: "stale" }))).toEqual({
      kind: "none",
    });
  });

  // @decision(DL-060)
  it("ages to now while unfinished and to its stop once finished", () => {
    const now = new Date("2026-09-26T11:00:00Z");
    expect(jobAge(job({}), now)).toBe(3600);
    expect(jobAge(job({ stopped_at: "2026-09-26T10:10:00Z" }), now)).toBe(600);
    expect(jobAge(job({ created_at: "nonsense" }), now)).toBe(0);
  });
});

// @decision(DL-060)
describe("the concurrency limits", () => {
  const session = (state: string, job?: string) =>
    ({ id: state + (job ?? ""), state, ...(job ? { job: { id: "j", kind: "pr", state: job } } : {}) }) as unknown as Session;

  it("every state but the finished ones counts against the job limit, queued included", () => {
    expect([...UNFINISHED].sort()).toEqual(["blocked", "delivering", "provisioning", "queued", "running", "steered"]);
    expect(unfinished("succeeded")).toBe(false);
    expect(unfinished(undefined)).toBe(false);
  });

  it("a job's session counts as its job, never as a session", () => {
    expect(
      limitUse([
        session("running"),
        session("starting"),
        session("stopped"),
        session("running", "running"),
        session("starting", "queued"),
        session("running", "blocked"),
        // A finished job whose sandbox has not stopped yet is a session.
        session("running", "succeeded"),
        session("stopped", "failed"),
      ]),
    ).toEqual({ sessions: 3, jobs: 3 });
  });
});
