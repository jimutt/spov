import { describe, expect, it } from "vitest";
import { ApiError } from "./api/client";
import { CANCELLABLE, cancellable, endable, ended } from "./jobcontrols";
import { KNOWN_JOB_STATES } from "./steer";

describe("which job can be cancelled from the page", () => {
  // @decision(DL-057)
  it("offers cancel until the run is over, and not while it is judged", () => {
    const offered = KNOWN_JOB_STATES.filter((st) => cancellable(st));
    expect(offered).toEqual([
      "queued",
      "provisioning",
      "running",
      "blocked",
      "steered",
    ]);
    expect(cancellable("delivering")).toBe(false);
  });

  // @decision(DL-057)
  it("offers nothing for no job or a state it does not know", () => {
    expect(cancellable(undefined)).toBe(false);
    expect(cancellable("hibernating")).toBe(false);
    expect(cancellable("toString")).toBe(false);
  });

  // @decision(DL-057)
  it("decides every state the page knows", () => {
    expect(Object.keys(CANCELLABLE).sort()).toEqual(
      [...KNOWN_JOB_STATES].sort(),
    );
  });
});

describe("which session can be ended from the page", () => {
  // @decision(DL-057)
  it("ends a live session no unfinished job drives", () => {
    expect(endable("running", true, undefined)).toBe(true);
    expect(endable("starting", true, undefined)).toBe(true);
    expect(endable("running", true, "succeeded")).toBe(true);
    expect(endable("running", true, "cancelled")).toBe(true);
  });

  // @decision(DL-057)
  it("never ends a job's session while the job is unfinished", () => {
    for (const st of [
      "queued",
      "provisioning",
      "running",
      "delivering",
      "blocked",
      "steered",
    ]) {
      expect(endable("running", true, st), st).toBe(false);
    }
  });

  // @decision(DL-057)
  it("waits until it knows whether a job drives the session", () => {
    expect(endable("running", false, undefined)).toBe(false);
  });

  // @decision(DL-057)
  it("has nothing to end once the session is over", () => {
    expect(endable("stopped", true, undefined)).toBe(false);
    expect(endable("failed", true, undefined)).toBe(false);
    expect(endable(undefined, true, undefined)).toBe(false);
  });
});

describe("sending the act", () => {
  // @decision(DL-057)
  it("throws the server's refusal", async () => {
    const problem = { title: "Conflict", status: 409, detail: "it finished" };
    const err = await ended(async () => ({
      response: { ok: false, status: 409 },
      error: problem,
    })).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).problem?.detail).toBe("it finished");
  });

  // @decision(DL-057)
  it("resolves on success", async () => {
    await expect(
      ended(async () => ({ response: { ok: true, status: 204 } })),
    ).resolves.toBeUndefined();
  });
});
