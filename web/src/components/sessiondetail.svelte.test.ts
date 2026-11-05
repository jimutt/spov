import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The session page, mounted, for the composer's wiring alone: that it
 * is rendered, that an echo reaches the transcript, and that the echo
 * withdraws when the session says the words back.
 *
 * @decision(DL-059)
 */

// jsdom implements no layout and no scrolling, so `Element.scrollTo` is
// simply absent. The tail latch calls it whenever the transcript grows
// — which is exactly what an arriving echo does — and the call sits
// inside a requestAnimationFrame callback, where nothing catches the
// TypeError: vitest reports every test passing, prints an unhandled
// error and exits 1, which reads as a flake rather than as a missing
// stub.
if (!Element.prototype.scrollTo) {
  Element.prototype.scrollTo = () => {};
}

const SESSION = {
  id: "ses_1",
  name: "live",
  org_id: "o",
  owner_id: "u",
  state: "running",
  created_at: "2026-09-16T10:00:00Z",
  updated_at: "2026-09-16T10:00:00Z",
  repos: [],
};

const stub = vi.hoisted(() => {
  const encoder = new TextEncoder();
  let log!: ReadableStreamDefaultController<Uint8Array>;
  const bodies: Record<string, unknown>[] = [];
  let answer: Record<string, unknown> = {
    id: "msg_1",
    conversation: "ses_1",
    delivery: "steer",
  };
  let status = 202;
  let state = "running";
  let jobs: Record<string, unknown>[] = [];
  let jobsAnswer = true;
  let credentials: Record<string, unknown>[] | undefined;

  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });

  const posted: string[] = [];
  const files: string[] = [];
  let holdFiles = false;
  const held: (() => void)[] = [];
  const deleted: string[] = [];
  let jobReads = 0;
  let deleteAnswer: { status: number; problem?: Record<string, unknown> } = { status: 204 };
  const fetched = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    if (req.method === "DELETE") {
      deleted.push(url.pathname);
      if (deleteAnswer.status !== 204) return json(deleteAnswer.problem ?? {}, deleteAnswer.status);
      // The act took: the next read finds it done.
      if (url.pathname.startsWith("/api/v1/jobs/")) jobs = jobs.map((j) => ({ ...j, state: "cancelled" }));
      else state = "stopped";
      return new Response(null, { status: 204 });
    }
    if (req.method === "POST") {
      posted.push(url.pathname);
      // A resume has no body at all, and JSON.parse("") throws — which
      // would surface as the page's own failure rather than as the
      // stub's.
      const raw = await req.text();
      bodies.push((raw ? JSON.parse(raw) : {}) as Record<string, unknown>);
      if (url.pathname.endsWith("/resume")) return new Response(null, { status: 204 });
      if (url.pathname.endsWith("/interrupt")) {
        return json({ interrupted: true, conversation: "ses_1" }, 200);
      }
      return json(answer, status);
    }
    if (url.pathname.endsWith("/log")) {
      // Never closes, so the page stays live for the whole test and the
      // test decides what it is told.
      return new Response(
        new ReadableStream<Uint8Array>({
          start(c) {
            log = c;
          },
        }),
        { status: 200, headers: { "content-type": "text/event-stream" } },
      );
    }
    if (url.pathname.endsWith("/messages")) return json({ conversations: [] });
    if (url.pathname === "/api/v1/jobs/j1/file") {
      const path = url.searchParams.get("path") ?? "";
      files.push(path);
      const answer = () =>
        new Response(path === "findings.md" ? "# Findings\n\nAll ![chart](images/c.png) here." : `# Body of ${path}`, {
          status: 200,
          headers: { "content-type": "text/markdown; charset=utf-8" },
        });
      if (holdFiles) return new Promise<Response>((resolve) => held.push(() => resolve(answer())));
      return answer();
    }
    if (url.pathname === "/api/v1/jobs") {
      jobReads++;
      // Never answers, so "the page does not know yet" is a state an
      // assertion can hold.
      if (!jobsAnswer) return new Promise<Response>(() => {});
      return json({ jobs });
    }
    if (url.pathname.startsWith("/api/v1/sessions/")) return json({ ...SESSION, state, credentials });
    return new Response(null, { status: 404 });
  });
  globalThis.fetch = fetched as unknown as typeof globalThis.fetch;

  return {
    bodies: () => bodies,
    files: () => files,
    holdFiles: (on: boolean) => (holdFiles = on),
    // Newest first, so a stale answer arrives last.
    release: () => held.splice(0).reverse().forEach((r) => r()),
    posted: () => posted,
    deleted: () => deleted,
    jobReads: () => jobReads,
    refuseDelete: (status: number, problem: Record<string, unknown>) =>
      (deleteAnswer = { status, problem }),
    reset: () => {
      bodies.splice(0, bodies.length);
      posted.splice(0, posted.length);
      files.splice(0, files.length);
      holdFiles = false;
      held.splice(0);
      deleted.splice(0, deleted.length);
      deleteAnswer = { status: 204 };
      jobReads = 0;
      credentials = undefined;
    },
    setAnswer: (a: Record<string, unknown>) => (answer = a),
    refuse: (s: number, problem: Record<string, unknown>) => {
      status = s;
      answer = problem;
    },
    accept: () => {
      status = 202;
      answer = { id: "msg_1", conversation: "ses_1", delivery: "steer" };
      state = "running";
      jobs = [];
      jobsAnswer = true;
    },
    holdJobs: () => (jobsAnswer = false),
    withCredentials: (c: Record<string, unknown>[]) => (credentials = c),
    sessionState: (s: string) => (state = s),
    withJob: (jobState: string, extra: Record<string, unknown> = {}) =>
      (jobs = [
        {
          id: "j1",
          session_id: "ses_1",
          owner_id: "u",
          kind: "report",
          goal: "g",
          state: jobState,
          attempt: 1,
          created_at: "2026-09-16T10:00:00Z",
          updated_at: "2026-09-16T10:00:00Z",
          ...extra,
        },
      ]),
    /** Push one log frame, as the server's SSE encoder writes it. */
    say: (data: unknown) =>
      log.enqueue(encoder.encode(`event: log\ndata: ${JSON.stringify(data)}\n\n`)),
    /**
     * The server saying this session will produce nothing more, which
     * is `reason: "stopped"` and not the five-minute `lifetime` close.
     */
    endTheStream: () => {
      log.enqueue(
        encoder.encode(
          `event: end\ndata: ${JSON.stringify({ reason: "stopped", at: "2026-09-16T10:02:00Z" })}\n\n`,
        ),
      );
      log.close();
    },
  };
});

import SessionDetail from "./SessionDetail.svelte";

beforeEach(() => {
  stub.reset();
  stub.accept();
});

async function settle() {
  for (let i = 0; i < 60; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  for (let i = 0; i < 20; i++) await Promise.resolve();
  flushSync();
}

async function open() {
  history.replaceState({}, "", "/session/?id=ses_1");
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(SessionDetail, { target });
  await settle();
  return {
    target,
    box: () => target.querySelector("#composer-text") as HTMLTextAreaElement | null,
    queue: () => target.querySelector('[data-testid="composer-queue"]'),
    refusal: () => target.querySelector('[data-testid="composer-refusal"]')?.textContent ?? "",
    send: () => target.querySelector('[data-testid="composer-send"]') as HTMLButtonElement,
    stop: () => target.querySelector('[data-testid="composer-stop"]') as HTMLButtonElement | null,
    resume: () =>
      target.querySelector('[data-testid="composer-resume"]') as HTMLButtonElement | null,
    notes: () =>
      Array.from(target.querySelectorAll('[data-testid="echo-note"]')).map((n) => n.textContent),
    said: () => target.textContent ?? "",
    control: (id: string) =>
      target.querySelector(`[data-testid="${id}"]`) as HTMLButtonElement | null,
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("the composer on the session page", () => {
  // @decision(DL-059)
  it("draws what was sent and stops drawing it when the session says it back", async () => {
    const page = await open();
    const box = page.box();
    expect(box, "the composer is not on the session page").not.toBeNull();

    box!.value = "try the other branch";
    box!.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    page.send().click();
    await settle();

    // The message went out, under the id this browser minted.
    expect(stub.bodies()).toHaveLength(1);
    const id = String(stub.bodies()[0].id);
    expect(id).toMatch(/^msg_/);

    // In the transcript, marked as not yet recorded.
    expect(page.said()).toContain("try the other branch");
    expect(page.notes()).toHaveLength(1);
    expect(page.notes()[0]).toContain("transcript");

    // The recorder catches up on the same id, and the echo goes.
    stub.say({
      name: "session.inbox.enqueued",
      source: "opencode",
      role: "user",
      message: id,
      text: "try the other branch",
      at: "2026-09-16T10:01:00Z",
      pos: 1,
    });
    await settle();
    expect(page.notes()).toEqual([]);
    expect(page.said().split("try the other branch")).toHaveLength(2);
    page.done();
  });

  // @decision(DL-059) @decision(DL-019)
  it("reconciles on the id the server reported", async () => {
    stub.setAnswer({ id: "msg_theirs", conversation: "ses_1", delivery: "steer" });
    const page = await open();
    page.box()!.value = "hello";
    page.box()!.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    page.send().click();
    await settle();
    expect(page.notes()).toHaveLength(1);

    stub.say({
      name: "session.inbox.enqueued",
      source: "opencode",
      role: "user",
      message: "msg_theirs",
      text: "hello",
      at: "2026-09-16T10:01:00Z",
      pos: 1,
    });
    await settle();
    expect(page.notes()).toEqual([]);
    page.done();
  });

  // @decision(DL-059)
  it("withdraws the echo and gives the words back when the send is refused", async () => {
    stub.refuse(409, { title: "the job is blocked, so it cannot take a message" });
    const page = await open();
    page.box()!.value = "are you stuck?";
    page.box()!.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    page.send().click();
    await settle();

    expect(stub.bodies()).toHaveLength(1);
    expect(page.notes()).toEqual([]);
    expect(page.said()).toContain("the job is blocked");
    expect(page.box()!.value).toBe("are you stuck?");
    page.done();
  });

  // @decision(DL-059)
  it("draws nothing on a session that is not running", async () => {
    stub.sessionState("stopped");
    const page = await open();
    expect(page.box()).toBeNull();
    page.done();
  });

  // The other half of `composerShown`: only the stream's own end says
  // the session is over, since the row is never refreshed once it
  // settles.
  //
  // @decision(DL-059) @decision(DL-018)
  it("stops drawing the composer when the stream says the session is over", async () => {
    const page = await open();
    expect(page.box()).not.toBeNull();
    stub.endTheStream();
    await settle();
    expect(page.box()).toBeNull();
    page.done();
  });

  // The row is polled only while starting, so the end of the stream is
  // what tells the header the session is over.
  //
  // @decision(DL-017) @decision(DL-018)
  it("re-reads the session when the stream says it is over", async () => {
    const page = await open();
    expect(page.said()).toMatch(/\brunning\b/i);
    stub.sessionState("stopped");
    stub.endTheStream();
    await settle();
    expect(page.said()).toMatch(/\bstopped\b/i);
    expect(page.said()).not.toMatch(/\brunning\b/i);
    page.done();
  });

  // @decision(DL-059) @decision(DL-052)
  it("tells the composer what state the session's job is in", async () => {
    stub.withJob("blocked");
    const blocked = await open();
    expect(blocked.box()).toBeNull();
    expect(blocked.refusal()).toContain("parked on a question");
    blocked.done();

    stub.withJob("running");
    const running = await open();
    expect(running.box()).not.toBeNull();
    // And a job is never offered queueing.
    expect(running.queue()).toBeNull();
    running.done();
  });

  // In-flight is derived on this page, from the log it already holds,
  // and handed to the composer.
  //
  // @decision(DL-059) @decision(DL-053)
  it("draws Stop only once the stream says a turn has started", async () => {
    const page = await open();
    expect(page.stop(), "Stop was drawn on a session with no turn running").toBeNull();

    stub.say({
      name: "session.execution.started",
      source: "opencode",
      at: "2026-09-16T10:01:00Z",
      pos: 1,
    });
    await settle();
    expect(page.stop(), "a turn started and Stop never appeared").not.toBeNull();

    // Goes when the turn does, however it ended.
    stub.say({
      name: "session.execution.interrupted",
      source: "opencode",
      at: "2026-09-16T10:01:30Z",
      pos: 2,
    });
    await settle();
    expect(page.stop()).toBeNull();
    page.done();
  });

  // @decision(DL-059) @decision(DL-053)
  it("stops the session it is looking at", async () => {
    const page = await open();
    stub.say({
      name: "session.execution.started",
      source: "opencode",
      at: "2026-09-16T10:01:00Z",
      pos: 1,
    });
    await settle();
    page.stop()!.click();
    await settle();
    expect(stub.posted()).toEqual(["/api/v1/sessions/ses_1/interrupt"]);
    expect(page.said()).toContain("The turn was interrupted");

    // The next turn starting clears the outcome line.
    stub.say({
      name: "session.execution.interrupted",
      source: "opencode",
      at: "2026-09-16T10:01:30Z",
      pos: 2,
    });
    await settle();
    expect(page.said(), "the outcome went as soon as the turn ended").toContain(
      "The turn was interrupted",
    );
    stub.say({
      name: "session.execution.started",
      source: "opencode",
      at: "2026-09-16T10:02:00Z",
      pos: 3,
    });
    await settle();
    expect(page.said()).not.toContain("The turn was interrupted");
    page.done();
  });

  // @decision(DL-059) @decision(DL-054)
  it("hands the composer the job to resume", async () => {
    stub.withJob("steered");
    const page = await open();
    expect(page.resume(), "a steered job was offered no way back").not.toBeNull();
    page.resume()!.click();
    await settle();
    expect(stub.posted()).toEqual(["/api/v1/jobs/j1/resume"]);
    page.done();
  });

  // "No job" and "the job listing has not answered" are the same
  // `undefined`.
  //
  // @decision(DL-059) @decision(DL-052) @decision(DL-057)
  it("offers nothing about a job it has not read yet", async () => {
    stub.holdJobs();
    const page = await open();
    expect(page.box()).not.toBeNull();
    expect(page.queue()).toBeNull();
    page.done();
  });
});

describe("an ask on the session page", () => {
  // The wiring only: the card is drawn from the log, the answer reaches
  // the server, and the log settles it. The rest is asks.test.ts.
  //
  // @decision(DL-020)
  it("draws a pending permission, sends Allow once, and settles on the reply", async () => {
    const page = await open();
    const ask = { id: "per_1", kind: "permission", action: "glob", resources: ["*.txt"], call: "call_1" };
    stub.say({ name: "permission.asked", source: "ephemeral", ask, at: "2026-09-16T10:01:00Z", pos: 1 });
    await settle();
    expect(page.control("ask-permission")?.textContent).toContain("glob *.txt");
    expect(page.control("composer-status")?.textContent).toContain("Waiting on your answer");

    page.control("ask-allow")!.click();
    await settle();
    expect(stub.posted()).toContain("/api/v1/sessions/ses_1/permissions/per_1");
    expect(stub.bodies().at(-1)).toEqual({ decision: "once" });

    stub.say({
      name: "permission.replied",
      source: "ephemeral",
      ask: { id: "per_1", kind: "permission", reply: "once" },
      at: "2026-09-16T10:01:05Z",
      pos: 2,
    });
    await settle();
    expect(page.control("ask-permission")).toBeNull();
    expect(page.control("ask-settled")?.textContent).toContain("Allowed once");
    expect(page.said()).not.toContain("Waiting on your answer");
    page.done();
  });
});

describe("the session's model on its page", () => {
  // @decision(DL-014)
  it("shows the model the log last selected beside Send", async () => {
    const page = await open();
    stub.say({
      name: "session.model.selected",
      source: "opencode",
      model: "anthropic/claude-opus-5",
      at: "2026-09-16T10:01:00Z",
      pos: 1,
    });
    await settle();
    expect(page.control("model-switch")?.textContent).toContain("anthropic/claude-opus-5");
    page.done();
  });
});

describe("ending from the session page", () => {
  // @decision(DL-057)
  it("cancels a running job only on the second press", async () => {
    stub.withJob("running");
    const page = await open();
    expect(page.control("end-session"), "a job's session offers End session").toBeNull();

    page.control("cancel-job")!.click();
    await settle();
    expect(stub.deleted()).toEqual([]);
    expect(page.said()).toContain("ends without a verdict");
    expect((document.activeElement as HTMLElement | null)?.dataset.testid).toBe("cancel-job-keep");

    page.control("cancel-job-yes")!.click();
    page.control("cancel-job-yes")?.click();
    await settle();
    expect(stub.deleted()).toEqual(["/api/v1/jobs/j1"]);
    // The page re-read the job, which is now cancelled.
    expect(page.control("cancel-job")).toBeNull();
    expect(page.control("cancel-job-ask")).toBeNull();
    page.done();
  });

  // @decision(DL-057)
  it("offers no cancel while a job is being judged", async () => {
    stub.withJob("delivering");
    const page = await open();
    expect(page.control("cancel-job")).toBeNull();
    page.done();
  });

  // @decision(DL-057)
  it("ends a plain session, and Escape backs out first", async () => {
    const page = await open();
    expect(page.control("cancel-job")).toBeNull();

    page.control("end-session")!.click();
    await settle();
    page
      .control("end-session-keep")!
      .dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await settle();
    expect(page.control("end-session-ask")).toBeNull();
    expect(stub.deleted()).toEqual([]);

    page.control("end-session")!.click();
    await settle();
    page.control("end-session-yes")!.click();
    await settle();
    expect(stub.deleted()).toEqual(["/api/v1/sessions/ses_1"]);
    page.done();
  });

  // @decision(DL-057)
  it("offers End session only once it knows no job drives the session", async () => {
    stub.holdJobs();
    const page = await open();
    expect(page.control("end-session")).toBeNull();
    page.done();
  });

  // @decision(DL-057)
  it("shows a refusal beside the job, re-reads, and does not retry", async () => {
    stub.withJob("running");
    stub.refuseDelete(409, {
      title: "Conflict",
      status: 409,
      detail: "job not cancellable: it finished while the cancel was in flight",
    });
    const page = await open();
    page.control("cancel-job")!.click();
    await settle();
    const reads = stub.jobReads();
    page.control("cancel-job-yes")!.click();
    await settle();
    expect(stub.deleted()).toHaveLength(1);
    expect(stub.jobReads()).toBeGreaterThan(reads);
    expect(page.said()).toContain("it finished while the cancel was in flight");
    page.done();
  });
});

describe("what the sandbox holds", () => {
  // @decision(DL-043)
  it("names each platform's credential kind in the workspace section", async () => {
    stub.withCredentials([
      { platform: "github", kind: "renewed" },
      { platform: "codefloe", kind: "stored" },
    ]);
    const page = await open();
    const lines = () =>
      Array.from(page.target.querySelectorAll('[data-testid="sandbox-credential"]')).map(
        (n) => n.textContent?.replace(/\s+/g, " ").trim() ?? "",
      );
    expect(lines()).toHaveLength(2);
    expect(lines()[0]).toContain("github");
    expect(lines()[0]).toContain("App token");
    expect(lines()[1]).toContain("codefloe");
    // The full sentence is on demand. @decision(DL-057)
    expect(lines()[1]).not.toContain("cannot check");
    const about = page.target.querySelector<HTMLButtonElement>('button[aria-label="About the codefloe credential"]');
    expect(about, "no control opens the credential's note").not.toBeNull();
    about!.click();
    await settle();
    expect(lines()[1]).toContain("cannot check");
    expect(lines()[0]).not.toContain("renewed while");
    page.done();
  });
});

describe("the verdict", () => {
  const gates = (...outcomes: string[]) =>
    outcomes.map((outcome, i) => ({ gate: ["check", "deliverable", "conduct"][i], outcome, detail: `d${i}` }));
  const verdict = (page: { target: HTMLElement }) =>
    page.target.querySelector<HTMLDetailsElement>('[data-testid="verdict"]');

  // @decision(DL-057)
  it("is folded to one status when every gate passed, with the gates inside", async () => {
    stub.withJob("succeeded", { gates: gates("unverified", "passed", "passed") });
    const page = await open();
    const v = verdict(page);
    expect(v, "no verdict drawn").not.toBeNull();
    expect(v!.open).toBe(false);
    expect(v!.querySelector("summary")?.textContent?.replace(/\s+/g, " ")).toContain("Passed, 1 unverified");
    expect(v!.textContent).toContain("Deliverable");
    page.done();
  });

  // @decision(DL-057)
  it("opens by itself when a gate failed", async () => {
    stub.withJob("failed", { gates: gates("failed", "passed", "passed") });
    const page = await open();
    expect(verdict(page)?.open).toBe(true);
    expect(verdict(page)?.querySelector("summary")?.textContent).toContain("1 gate failed");
    page.done();
  });
});

describe("what the job delivered", () => {
  // @decision(DL-051) @decision(DL-063) @decision(DL-050)
  it("lists each report, and reads one from the file endpoint only when asked", async () => {
    stub.withJob("succeeded", {
      partial: true,
      unfinished: "the migration notes",
      deliverables: [
        { kind: "report", status: "verified", path: "findings.md", title: "Findings", size: 2048 },
        { kind: "report", status: "over_limit", path: "big.md", title: "big", detail: "larger than 10 MiB" },
      ],
    });
    const page = await open();
    const list = page.target.querySelector('[data-testid="deliverables"]')?.textContent ?? "";
    expect(list).toContain("Findings");
    expect(list).toContain("Too large");
    expect(page.said()).toContain("the migration notes");
    expect(page.said()).toContain("part of what was asked");
    expect(stub.files(), "a body was fetched before anyone asked").toEqual([]);

    const read = Array.from(page.target.querySelectorAll("button")).find((b) => b.textContent?.trim() === "Read");
    expect(read, "the collected report offers no Read").toBeDefined();
    read!.click();
    await settle();
    expect(stub.files()).toEqual(["findings.md"]);
    const dialog = page.target.querySelector('[role="dialog"]');
    expect(dialog?.querySelector("h1")?.textContent).toBe("Findings");
    expect(dialog?.querySelector("img")?.getAttribute("src")).toContain("/api/v1/jobs/j1/file?path=images%2Fc.png");
    const download = Array.from(dialog?.querySelectorAll("a") ?? []).find((a) => a.textContent?.trim() === "Download");
    expect(download?.getAttribute("href")).toBe("/api/v1/jobs/j1/file?path=findings.md&download=true");
    page.done();
  });
});

describe("reading one report after another", () => {
  // @decision(DL-051)
  it("never shows a body that arrives after another report was opened", async () => {
    stub.withJob("succeeded", {
      deliverables: [
        { kind: "report", status: "verified", path: "a.md", title: "A" },
        { kind: "report", status: "verified", path: "b.md", title: "B" },
      ],
    });
    const page = await open();
    const reads = () => Array.from(page.target.querySelectorAll("button")).filter((b) => b.textContent?.trim() === "Read");
    stub.holdFiles(true);
    reads()[0].click();
    await settle();
    reads()[1].click();
    await settle();
    stub.release();
    await settle();
    const dialog = page.target.querySelector('[role="dialog"]');
    expect(dialog?.querySelector("h2")?.textContent?.trim()).toBe("B");
    expect(dialog?.textContent).toContain("Body of b.md");
    expect(dialog?.textContent).not.toContain("Body of a.md");
    page.done();
  });
});
