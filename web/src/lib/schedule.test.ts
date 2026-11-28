import { describe, expect, it } from "vitest";
import { emptyDraft } from "./jobform";
import {
  cadenceOf,
  cadenceWords,
  cronOf,
  defaultCadence,
  draftOf,
  editedCadence,
  jobPage,
  outcomeWord,
  pausedLine,
  scheduleJobOf,
  scheduleProblems,
  toScheduleRequest,
} from "./schedule";

/**
 * The schedule pages' logic: named cadences to cron and back, the words
 * for one, what stops a save, and the job an edit sends.
 *
 * @decision(DL-195)
 */
describe("a schedule's cadence", () => {
  it("builds cron from a named choice and reads it back", () => {
    const base = defaultCadence();
    for (const [c, cron] of [
      [{ ...base, kind: "hourly", minute: 15 }, "15 * * * *"],
      [{ ...base, kind: "daily", hour: 9, minute: 30 }, "30 9 * * *"],
      [{ ...base, kind: "weekly", weekday: 1, hour: 6, minute: 0 }, "0 6 * * 1"],
      [{ ...base, kind: "monthly", day: 28, hour: 23, minute: 59 }, "59 23 28 * *"],
    ] as const) {
      expect(cronOf(c)).toBe(cron);
      const back = cadenceOf(cron);
      expect(back.kind).toBe(c.kind);
      expect(cronOf(back)).toBe(cron);
    }
  });

  it("reads the descriptors as named choices and anything else as custom", () => {
    expect(cronOf(cadenceOf("@daily"))).toBe("0 0 * * *");
    expect(cadenceOf("@weekly").kind).toBe("weekly");
    for (const cron of ["0 9 * * 1-5", "0 */2 * * *", "0 9 30 * *", "0 9 * 6 *", "nonsense"]) {
      const c = cadenceOf(cron);
      expect(c.kind).toBe("custom");
      expect(cronOf(c)).toBe(cron);
    }
  });

  it("says a cadence in words with its zone", () => {
    expect(cadenceWords("30 9 * * *", "Europe/Stockholm")).toBe("Every day at 09:30 (Europe/Stockholm)");
    expect(cadenceWords("0 6 * * 1", "UTC")).toBe("Every Monday at 06:00 (UTC)");
    expect(cadenceWords("5 * * * *", "UTC")).toBe("Every hour at :05");
    expect(cadenceWords("0 0 1 * *", "Asia/Tokyo")).toBe("On day 1 of every month at 00:00 (Asia/Tokyo)");
    expect(cadenceWords("0 9 * * 1-5", "UTC")).toBe("0 9 * * 1-5 (UTC)");
  });
});

describe("a schedule's save", () => {
  const job = { ...emptyDraft(), goal: "report on stale deps", repos: [{ platform: "github", owner: "o", name: "r" }] };

  it("needs a name of the server's shape and a cron when custom", () => {
    expect(scheduleProblems("weekly-deps", defaultCadence(), job)).toEqual([]);
    expect(scheduleProblems("Weekly Deps", defaultCadence(), job)[0]).toContain("name");
    expect(scheduleProblems("w", { ...defaultCadence(), kind: "custom", cron: " " }, job)).toContain(
      "a custom cadence needs a cron expression",
    );
    expect(scheduleProblems("w", defaultCadence(), emptyDraft()).length).toBeGreaterThan(0);
  });

  it("sends the cadence, the zone and the job form's request", () => {
    const body = toScheduleRequest(" weekly ", defaultCadence(), "", { ...job, model: "anthropic/m", mode: "high" });
    expect(body).toEqual({
      name: "weekly",
      cron: "0 9 * * *",
      zone: "UTC",
      job: { goal: "report on stale deps", kind: "report", repos: job.repos, model: "anthropic/m#high" },
    });
  });

  it("keeps what the CLI set beside the form's fields on an edit", () => {
    const stored = {
      goal: "old",
      kind: "pr" as const,
      repos: job.repos,
      model: "anthropic/old",
      check: "./r/check.sh",
      env: { STAGE: "x" },
      landing: "r",
      reasoning: "low",
    };
    const sent = scheduleJobOf({ ...job, model: "anthropic/new" }, stored);
    expect(sent).toEqual({
      goal: "report on stale deps",
      kind: "report",
      repos: job.repos,
      model: "anthropic/new",
      env: { STAGE: "x" },
      landing: "r",
    });
    // A field the form shows and the person cleared is cleared.
    expect("check" in sent).toBe(false);
    // Without a model in the form, the CLI's reasoning stays.
    expect(scheduleJobOf({ ...job, model: "" }, stored).reasoning).toBe("low");
  });

  it("fills the form from a stored job, the mode split off its model", () => {
    const d = draftOf({ goal: "g", kind: "pr", repos: job.repos, model: "anthropic/m#high", max_duration: "2h", git_access: "all" });
    expect(d).toMatchObject({ goal: "g", kind: "pr", model: "anthropic/m", mode: "high", clock: "2h", access: "all", check: "" });
  });
});

describe("an edit", () => {
  const repos = [{ platform: "github", owner: "o", name: "r" }];

  it("keeps a mode the CLI sent as reasoning when nothing changes", () => {
    const stored = { goal: "g", kind: "report" as const, repos, model: "anthropic/x", reasoning: "low" };
    const d = draftOf(stored);
    expect(d.mode).toBe("low");
    expect(scheduleJobOf(d, stored)).toMatchObject({ model: "anthropic/x#low" });
    expect("reasoning" in scheduleJobOf(d, stored)).toBe(false);
  });

  it("drops a preference once the person chose another value", () => {
    const stored = { goal: "g", kind: "report" as const, repos, max_duration: "2h", max_duration_preferred: true, git_access: "all" as const, git_access_preferred: true };
    const same = scheduleJobOf(draftOf(stored), stored);
    expect(same).toMatchObject({ max_duration_preferred: true, git_access_preferred: true });
    const changed = scheduleJobOf({ ...draftOf(stored), clock: "4h", access: "" }, stored);
    expect("max_duration_preferred" in changed).toBe(false);
    expect("git_access_preferred" in changed).toBe(false);
  });

  it("sends the cadence and zone only when they changed", () => {
    const stored = { cron: "@daily", zone: "UTC" };
    expect(editedCadence(stored, cadenceOf("0 0 * * *"), "UTC")).toEqual({});
    expect(editedCadence(stored, cadenceOf("0 9 * * *"), " UTC ")).toEqual({ cron: "0 9 * * *" });
    expect(editedCadence(stored, cadenceOf("@daily"), "Europe/Stockholm")).toEqual({ zone: "Europe/Stockholm" });
  });
});

describe("a schedule's runs", () => {
  it("says each outcome in words and links a job by its session's name", () => {
    const at = { slot: "2026-11-27T09:00:00Z", fired_at: "2026-11-27T09:00:30Z" };
    expect(outcomeWord({ ...at, outcome: "started" })).toBe("Started");
    expect(outcomeWord({ ...at, outcome: "started", reason: "already run" })).toBe("Already run");
    expect(outcomeWord({ ...at, outcome: "missed", missed: 5 })).toBe("5 runs missed");
    expect(outcomeWord({ ...at, outcome: "skipped" })).toBe("Skipped");
    // The server names a job's session with the id in lower case.
    expect(jobPage("01M8HS2H1S35XJ5PGQ3Y0DX4HA")).toBe("/session/?id=job-01m8hs2h1s35xj5pgq3y0dx4ha");
    expect(pausedLine("paused by a@b.test")).toBe("Paused by a@b.test");
    expect(pausedLine("3 failed runs in a row")).toBe("Paused: 3 failed runs in a row");
    expect(pausedLine("")).toBe("Paused");
  });
});
