import { describe, expect, it } from "vitest";
import { ApiError } from "./api/client";
import { Committed } from "./jobform";
import {
  followsAResume,
  resumableLine,
  resumed,
  resumeOffered,
  resumeQuestion,
  watchesForAResume,
} from "./resume";

const snap = { mode: "snapshot" as const, until: "2026-11-13T09:00:00Z" };
const transcript = { mode: "transcript" as const };

describe("resume", () => {
  it("is offered only on a stopped or failed session with a choice", () => {
    expect(resumeOffered("stopped", snap)).toBe(true);
    expect(resumeOffered("failed", transcript)).toBe(true);
    expect(resumeOffered("running", snap)).toBe(false);
    expect(resumeOffered("stopped", undefined)).toBe(false);
    expect(watchesForAResume("stopped", snap)).toBe(true);
  });

  it("words the question by what will come back", () => {
    expect(resumeQuestion(snap, "en-US")).toContain("uncommitted changes included");
    expect(resumeQuestion(snap, "en-US")).toContain("Nov 13 at the latest");
    expect(resumeQuestion({ ...snap, pending: true })).toContain("still being taken");
    const t = resumeQuestion(transcript);
    expect(t).toContain("cloned fresh");
    expect(t).toContain("uncommitted changes and installed packages are gone");
  });

  it("notes a snapshot in the listing and nothing for a transcript", () => {
    expect(resumableLine(snap, "en-US")).toBe("Resumable until Nov 13");
    expect(resumableLine(transcript)).toBe("");
    expect(resumableLine(undefined)).toBe("");
  });

  it("reopens the live reads once a resume is under way", () => {
    expect(followsAResume(true, true, "starting")).toBe(true);
    expect(followsAResume(true, true, "running")).toBe(true);
    expect(followsAResume(false, true, "running")).toBe(false);
    expect(followsAResume(true, true, "stopped")).toBe(false);
    // The stream ended and the row has not caught up: not a resume.
    expect(followsAResume(true, false, "running")).toBe(false);
  });

  it("sends the message with the resume, and tells a refusal from an unreadable answer", async () => {
    let sent: unknown;
    await resumed(async (body) => {
      sent = body;
      return { response: { ok: true, status: 202 }, data: { state: "starting" } };
    }, { text: "go on", id: "msg_1" });
    expect(sent).toEqual({ message: { text: "go on", id: "msg_1" } });
    await expect(
      resumed(async () => ({ response: { ok: false, status: 409 }, error: { title: "no" } })),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(resumed(async () => ({ response: { ok: true, status: 202 } }))).rejects.toBeInstanceOf(Committed);
  });
});
