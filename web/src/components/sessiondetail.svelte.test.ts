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
  let resume: Record<string, unknown> | undefined;
  let session: Record<string, unknown> = {};
  let logOpens = 0;
  let conversations: unknown[] = [];

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
      // A session's resume answers with the session, now starting.
      if (url.pathname.startsWith("/api/v1/sessions/") && url.pathname.endsWith("/resume")) {
        state = "starting";
        resume = undefined;
        return json({ ...SESSION, state }, 202);
      }
      if (url.pathname.endsWith("/resume")) return new Response(null, { status: 204 });
      if (url.pathname.endsWith("/interrupt")) {
        return json({ interrupted: true, conversation: "ses_1" }, 200);
      }
      return json(answer, status);
    }
    if (url.pathname.endsWith("/log")) {
      logOpens++;
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
    if (url.pathname.endsWith("/messages")) return json({ conversations });
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
    if (url.pathname.startsWith("/api/v1/sessions/")) return json({ ...SESSION, ...session, state, credentials, resume });
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
    setSession: (fields: Record<string, unknown>) => (session = fields),
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
      resume = undefined;
      session = {};
      logOpens = 0;
      conversations = [];
    },
    withResume: (r: Record<string, unknown>) => (resume = r),
    logOpens: () => logOpens,
    requests: () => fetched.mock.calls.length,
    withConversations: (c: unknown[]) => (conversations = c),
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
import { Child, subagentConversations, subagentLog } from "../lib/subagent.fixture";
import { callFrames, childEnd, childStart } from "../lib/subagent.frames";

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

async function open(query = "") {
  history.replaceState({}, "", `/session/?id=ses_1${query}`);
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

describe("a subagent on the session page", () => {
  // The measured log of a session whose agent ran a subagent, replayed
  // through the page. The child's turn ends (pos 25) while the root's is
  // still running.
  //
  // @decision(DL-111) @decision(DL-059)
  it("keeps Stop drawn while a child's turn ends, and never draws its prompt as the reader's", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog.filter((e) => e.pos <= 25)) stub.say(ev);
    await settle();
    expect(page.stop(), "a subagent's turn ending took Stop away from the root's").not.toBeNull();
    expect(page.said()).not.toContain("You are a subagent spawned");
    // Its work is one card at the call, not lines among the agent's own.
    const card = page.target.querySelector('[data-testid="subagent"]');
    expect(card?.textContent).toContain("Review the retry change");
    expect(page.target.querySelectorAll('[data-testid="subagent"]')).toHaveLength(1);
    const bubbles = Array.from(page.target.querySelectorAll('[data-testid="message"]')).filter((m) =>
      m.className.includes("items-end"),
    );
    expect(bubbles).toHaveLength(1);

    // The root's own ending still takes it away.
    for (const ev of subagentLog.filter((e) => e.pos > 25)) stub.say(ev);
    await settle();
    expect(page.stop()).toBeNull();
    page.done();
  });
});

describe("a subagent's card on the session page", () => {
  // The card is drawn from the same measured log. The logic is in
  // timeline.test.ts and subagent.test.ts; this is the wiring: the card
  // is on the page in place of the call's lines, it opens onto the
  // child's own timeline, and an ask it raised is counted and reachable.
  //
  // @decision(DL-112)
  const card = (page: { target: HTMLElement }) => page.target.querySelector('[data-testid="subagent"]') as HTMLElement;
  const toggle = (page: { target: HTMLElement }) => card(page).querySelector("button[aria-expanded]") as HTMLButtonElement;

  it("is one quiet card with the child's state, closed over its conclusion, and opens onto its work", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog) stub.say(ev);
    await settle();

    // Neither a bubble nor an agent's or spov's line.
    expect(card(page).closest('[data-testid="message"]')).toBeNull();
    expect(card(page).querySelector('[data-testid="spov-tag"]')).toBeNull();
    expect(card(page).textContent).toContain("Review the retry change");
    expect(card(page).textContent).toContain("general");
    expect(card(page).textContent).toContain("Done");
    expect(card(page).getAttribute("data-state")).toBe("done");
    expect(card(page).querySelector('[data-testid="subagent-summary"]')?.textContent).toBe("1 tool call");
    // Closed: what it concluded, and none of its work.
    expect(card(page).querySelector('[data-testid="subagent-final"]')?.textContent).toContain("The retry change is safe to merge.");
    expect(card(page).querySelector('[data-testid="subagent-work"]')).toBeNull();
    expect(page.said()).not.toContain("You are a subagent spawned");
    expect(toggle(page).getAttribute("aria-expanded")).toBe("false");

    toggle(page).click();
    await settle();
    expect(toggle(page).getAttribute("aria-expanded")).toBe("true");
    const work = card(page).querySelector('[data-testid="subagent-work"]')!;
    // The task as a quiet row, never as the reader's words, and the
    // child's own words once. (Still no bubble: the check is below.)
    expect(work.querySelector('[data-testid="subagent-prompt"]')?.textContent).toContain("Review the diff of this branch");
    expect(work.textContent).toContain("glob");
    expect(work.textContent).toContain("internal/client/*.go");
    expect(work.querySelectorAll('[data-testid="subagent-message"]')).toHaveLength(1);
    expect(card(page).querySelector('[data-testid="subagent-final"]')).toBeNull();
    // The bubbles on the page are still only the reader's one.
    const bubbles = Array.from(page.target.querySelectorAll('[data-testid="message"]')).filter((m) => m.className.includes("items-end"));
    expect(bubbles).toHaveLength(1);

    toggle(page).click();
    await settle();
    expect(card(page).querySelector('[data-testid="subagent-work"]')).toBeNull();
    page.done();
  });

  it("is delegating before the child exists, with nothing to open yet", async () => {
    stub.withConversations(subagentConversations.filter((c) => !c.parent));
    const page = await open();
    for (const ev of subagentLog.filter((e) => e.pos <= 9)) stub.say(ev);
    await settle();
    expect(card(page).getAttribute("data-state")).toBe("starting");
    expect(card(page).textContent).toContain("Delegating…");
    expect(card(page).textContent).toContain("Review the retry change");
    expect(card(page).querySelector("button[aria-expanded]")).toBeNull();
    page.done();
  });

  it("is running while the child works, with what it did last", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog.filter((e) => e.pos <= 18)) stub.say(ev);
    await settle();
    expect(card(page).getAttribute("data-state")).toBe("running");
    expect(card(page).textContent).toContain("Running");
    expect(card(page).querySelector('[data-testid="subagent-latest"]')?.textContent).toBe("glob internal/client/*.go");
    // Not yet concluded.
    expect(card(page).querySelector('[data-testid="subagent-final"]')).toBeNull();
    page.done();
  });

  it("counts an ask its child raised, names it, and scrolls to it from the badge", async () => {
    stub.withConversations(subagentConversations);
    const scrolled: Element[] = [];
    const prior = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = function (this: Element) {
      scrolled.push(this);
    };
    const page = await open();
    for (const ev of subagentLog.filter((e) => e.pos <= 18)) stub.say(ev);
    stub.say({
      name: "permission.asked",
      source: "ephemeral",
      session: Child,
      ask: { id: "per_c", kind: "permission", action: "edit", resources: ["internal/client/retry.go"], call: "call_x" },
      at: "2026-11-08T19:17:48Z",
      pos: 19,
    });
    await settle();

    // Where it was raised, it can be answered, and it says whose it is.
    const ask = page.target.querySelector('[data-testid="ask"]') as HTMLElement;
    expect(ask.querySelector('[data-testid="ask-from"]')?.textContent).toBe("From subagent “Review the retry change”");
    expect(page.control("composer-status")?.textContent).toContain("Waiting on your answer");
    // The card counts it and goes to it.
    const badge = card(page).querySelector('[data-testid="subagent-waiting"]') as HTMLButtonElement;
    expect(badge.textContent).toContain("1 waiting on you");
    badge.click();
    expect(scrolled).toEqual([ask]);

    // Answered: the badge goes.
    stub.say({
      name: "permission.replied",
      source: "ephemeral",
      session: Child,
      ask: { id: "per_c", kind: "permission", reply: "once" },
      at: "2026-11-08T19:17:49Z",
      pos: 20,
    });
    await settle();
    expect(card(page).querySelector('[data-testid="subagent-waiting"]')).toBeNull();
    Element.prototype.scrollIntoView = prior;
    page.done();
  });
});

describe("a subagent's own view", () => {
  // `agent=` selects it on the same page, from the conversations and log
  // the page holds. These assert markup state: jsdom has no layout. The
  // tree and the addresses are in subagentview.test.ts.
  //
  // @decision(DL-113)
  const find = (page: { target: HTMLElement }, id: string) => page.target.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;
  const all = (page: { target: HTMLElement }, id: string) => Array.from(page.target.querySelectorAll(`[data-testid="${id}"]`)) as HTMLElement[];
  const viewOf = (agent: string) => `/session/?id=ses_1&agent=${agent}`;
  const plain = () => new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });

  async function wait() {
    await new Promise((r) => setTimeout(r, 30));
    await settle();
  }

  it("draws the child's own transcript with a breadcrumb, and no composer in its place", async () => {
    stub.withConversations(subagentConversations);
    const page = await open(`&agent=${Child}`);
    for (const ev of subagentLog) stub.say(ev);
    await settle();

    // The way back and the heading.
    const back = find(page, "subagent-back") as HTMLAnchorElement;
    expect(back.getAttribute("href")).toBe("/session/?id=ses_1");
    expect(find(page, "subagent-crumb")?.textContent).toBe("Review the retry change");
    expect(find(page, "subagent-heading")?.textContent).toBe("Review the retry change");
    expect(find(page, "subagent-bar")?.textContent).toContain("Done");
    // Its prompt row and its words, not the main conversation's.
    expect(find(page, "subagent-prompt")?.textContent).toContain("Review the diff of this branch");
    expect(page.said()).toContain("The retry change is safe to merge.");
    expect(page.said()).not.toContain("Can you check that the retry change");
    expect(page.said()).not.toContain("You are a subagent spawned");
    expect(all(page, "message").filter((m) => m.className.includes("items-end"))).toHaveLength(0);
    // Read-only: no composer, no Stop, no Resume, and one line saying so.
    expect(page.box()).toBeNull();
    expect(page.stop()).toBeNull();
    expect(page.resume()).toBeNull();
    expect(find(page, "subagent-readonly")?.textContent).toContain("You are reading a subagent. Messages go to the main conversation");
    expect((find(page, "subagent-readonly")?.querySelector("a") as HTMLAnchorElement).getAttribute("href")).toBe("/session/?id=ses_1");
    // The sidebar is the session's, unchanged.
    expect(page.control("end-session")).not.toBeNull();
    // One child: nothing to step between.
    expect(find(page, "subagent-siblings")).toBeNull();
    page.done();
  });

  it("is reached from the card and the sidebar by links, without a reload or a request", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog) stub.say(ev);
    await settle();

    // Main view: the composer, the card's Open link, the sidebar's list.
    expect(page.box()).not.toBeNull();
    expect(find(page, "subagent-bar")).toBeNull();
    expect(find(page, "subagent-readonly")).toBeNull();
    const opener = find(page, "subagent-open") as HTMLAnchorElement;
    expect(opener.getAttribute("href")).toBe(viewOf(Child));
    const listed = all(page, "subagent-link");
    expect(listed).toHaveLength(1);
    expect(listed[0].getAttribute("href")).toBe(viewOf(Child));
    expect(listed[0].textContent).toContain("Review the retry change");
    expect(listed[0].textContent).toContain("Done");
    expect(listed[0].textContent).toContain("1 call");

    const opens = stub.logOpens();
    const requests = stub.requests();
    const event = plain();
    opener.dispatchEvent(event);
    await settle();
    expect(event.defaultPrevented, "the page left the navigation to the browser").toBe(true);
    expect(location.search).toBe(`?id=ses_1&agent=${Child}`);
    expect(page.box()).toBeNull();
    expect(find(page, "subagent-readonly")).not.toBeNull();
    expect(find(page, "subagent-crumb")?.textContent).toBe("Review the retry change");
    expect(all(page, "subagent-link")[0].getAttribute("aria-current")).toBe("page");
    expect(stub.logOpens(), "switching view reopened the log").toBe(opens);
    expect(stub.requests(), "switching view asked for something").toBe(requests);

    // Back to the main conversation by the breadcrumb: pushed too.
    find(page, "subagent-back")!.dispatchEvent(plain());
    await settle();
    expect(location.search).toBe("?id=ses_1");
    expect(page.box()).not.toBeNull();
    expect(find(page, "subagent-bar")).toBeNull();
    expect(stub.logOpens()).toBe(opens);
    expect(stub.requests()).toBe(requests);
    page.done();
  });

  it("leaves a modified click to the browser", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog) stub.say(ev);
    await settle();
    const event = new MouseEvent("click", { bubbles: true, cancelable: true, button: 0, ctrlKey: true });
    // jsdom would try to navigate on an unprevented link click; stopped
    // after the page's own handler has seen it.
    const stop = (e: Event) => e.preventDefault();
    document.addEventListener("click", stop);
    find(page, "subagent-open")!.dispatchEvent(event);
    document.removeEventListener("click", stop);
    await settle();
    expect(location.search).toBe("?id=ses_1");
    expect(page.box()).not.toBeNull();
    page.done();
  });

  it("follows Back and Forward through the views it pushed", async () => {
    stub.withConversations(subagentConversations);
    const page = await open();
    for (const ev of subagentLog) stub.say(ev);
    await settle();
    find(page, "subagent-open")!.dispatchEvent(plain());
    await settle();
    expect(page.box()).toBeNull();
    const opens = stub.logOpens();

    history.back();
    await wait();
    expect(location.search).toBe("?id=ses_1");
    expect(page.box()).not.toBeNull();
    expect(find(page, "subagent-bar")).toBeNull();

    history.forward();
    await wait();
    expect(location.search).toBe(`?id=ses_1&agent=${Child}`);
    expect(page.box()).toBeNull();
    expect(find(page, "subagent-crumb")?.textContent).toBe("Review the retry change");
    expect(stub.logOpens()).toBe(opens);
    page.done();
  });

  it("answers an id the session does not have with the main conversation and a note", async () => {
    stub.withConversations(subagentConversations);
    const page = await open("&agent=ses_nope");
    for (const ev of subagentLog) stub.say(ev);
    await settle();
    expect(find(page, "subagent-unknown")?.textContent).toContain("no subagent with that id");
    expect(find(page, "subagent-bar")).toBeNull();
    expect(page.box(), "the composer is not on the main conversation").not.toBeNull();
    expect(page.said()).toContain("Can you check that the retry change");
    expect(find(page, "subagent")).not.toBeNull();
    page.done();
  });

  it("does not flash the note while the conversations are still being read", async () => {
    stub.withConversations(subagentConversations);
    const page = await open(`&agent=${Child}`);
    // The conversations answered but no frame has arrived: the child is
    // known from its messages alone.
    expect(find(page, "subagent-unknown")).toBeNull();
    expect(find(page, "subagent-crumb")?.textContent).toBe("Review the retry change");
    page.done();
  });

  describe("among siblings and a child's own", () => {
    // Three subagents of the main conversation, the first with a helper
    // of its own: the shape an agent that fans out produces.
    const P = "ses_parent";
    const family = [
      { id: P, title: "main", messages: [{ id: "msg_p", role: "user", text: "fan out", at: "2026-11-08T19:00:00Z" }] },
      { id: "ses_c1", parent: P, title: "Review client", messages: [{ id: "msg_1", role: "user", text: "check client", at: "2026-11-08T19:00:10Z" }] },
      { id: "ses_g1", parent: "ses_c1", title: "Read the tests", messages: [{ id: "msg_g", role: "user", text: "read tests", at: "2026-11-08T19:00:20Z" }] },
      { id: "ses_c2", parent: P, title: "Review server", messages: [{ id: "msg_2", role: "user", text: "check server", at: "2026-11-08T19:00:30Z" }] },
      { id: "ses_c3", parent: P, title: "Review web", messages: [{ id: "msg_3", role: "user", text: "check web", at: "2026-11-08T19:00:40Z" }] },
    ];
    const frames = [
      ...callFrames(P, "call_a", 10, { description: "Review client", end: "success", child: "ses_c1" }),
      ...childStart("ses_c1", 20),
      ...callFrames("ses_c1", "call_h", 30, { description: "Read the tests", end: "success", child: "ses_g1" }),
      ...childStart("ses_g1", 40),
      ...childEnd("ses_g1", 50, "tests read"),
      ...childEnd("ses_c1", 60, "client reviewed"),
      ...callFrames(P, "call_b", 70, { description: "Review server", end: "success", child: "ses_c2" }),
      ...childStart("ses_c2", 80),
      ...callFrames(P, "call_c", 90, { description: "Review web", end: "success", child: "ses_c3" }),
      ...childStart("ses_c3", 100),
    ];

    it("lists them nested in the sidebar, and steps between the children of one parent", async () => {
      stub.withConversations(family);
      const page = await open("&agent=ses_c2");
      for (const ev of frames) stub.say(ev);
      await settle();

      const list = all(page, "subagent-link");
      expect(list.map((a) => a.querySelector("span")?.textContent)).toEqual(["Review client", "Read the tests", "Review server", "Review web"]);
      // One level of indent per depth.
      const indent = list.map((a) => (a.parentElement as HTMLElement).style.paddingLeft);
      expect(indent).toEqual(["0rem", "0.875rem", "0rem", "0rem"]);
      expect(list.map((a) => a.getAttribute("aria-current"))).toEqual([null, null, "page", null]);

      expect(find(page, "subagent-siblings")?.textContent).toContain("2 of 3");
      expect(find(page, "subagent-previous")?.getAttribute("href")).toBe(viewOf("ses_c1"));
      expect(find(page, "subagent-next")?.getAttribute("href")).toBe(viewOf("ses_c3"));
      // The helper is not a sibling of these.
      expect(find(page, "subagent-bar")?.innerHTML).not.toContain("ses_g1");

      const opens = stub.logOpens();
      find(page, "subagent-next")!.dispatchEvent(plain());
      await settle();
      expect(find(page, "subagent-crumb")?.textContent).toBe("Review web");
      expect(find(page, "subagent-next")).toBeNull();
      expect(find(page, "subagent-next-none")).not.toBeNull();
      expect(find(page, "subagent-previous")?.getAttribute("href")).toBe(viewOf("ses_c2"));
      expect(page.said()).toContain("check web");
      expect(page.said()).not.toContain("check server");
      expect(stub.logOpens()).toBe(opens);
      page.done();
    });

    it("reaches a child's own subagents as cards in its view, under a breadcrumb that goes up", async () => {
      stub.withConversations(family);
      const page = await open("&agent=ses_c1");
      for (const ev of frames) stub.say(ev);
      await settle();
      // Its helper is a card in its list, with an Open link of its own.
      const card = find(page, "subagent")!;
      expect(card.textContent).toContain("Read the tests");
      const opener = card.querySelector('[data-testid="subagent-open"]') as HTMLAnchorElement;
      expect(opener.getAttribute("href")).toBe(viewOf("ses_g1"));

      opener.dispatchEvent(plain());
      await settle();
      const crumbs = find(page, "subagent-bar")!;
      expect(crumbs.querySelector('[data-testid="subagent-ancestor"]')?.textContent).toBe("Review client");
      expect(crumbs.querySelector('[data-testid="subagent-ancestor"]')?.getAttribute("href")).toBe(viewOf("ses_c1"));
      expect(crumbs.querySelector('[data-testid="subagent-up"]')?.getAttribute("aria-label")).toBe("Up to Review client");
      expect(find(page, "subagent-crumb")?.textContent).toBe("Read the tests");
      expect(page.said()).toContain("read tests");
      // Alone under its parent: nothing to step between.
      expect(find(page, "subagent-siblings")).toBeNull();
      page.done();
    });
  });

  it("starts the window again on a change of view", async () => {
    // @decision(DL-070)
    stub.withConversations(subagentConversations);
    const page = await open();
    for (let i = 0; i < 250; i++) {
      stub.say({
        name: "session.text.ended",
        source: "opencode",
        text: `line ${i} said`,
        at: new Date(Date.UTC(2026, 8, 16, 10, 0, i)).toISOString(),
        pos: i + 1,
      });
    }
    await settle();
    const drawn = () => (page.said().match(/line \d+ said/g) ?? []).length;
    // The conversation's two messages and the card are blocks too.
    expect(drawn()).toBe(197);
    page.control("transcript-earlier")!.click();
    await settle();
    expect(drawn()).toBe(250);

    find(page, "subagent-open")!.dispatchEvent(plain());
    await settle();
    expect(drawn(), "the child's view drew the main conversation's lines").toBe(0);
    find(page, "subagent-back")!.dispatchEvent(plain());
    await settle();
    // Not the 250 the reader had opened: the newest window again.
    expect(drawn()).toBe(197);
    expect(page.control("transcript-earlier")?.textContent).toContain("Show earlier (53 more)");
    page.done();
  });

  it("says in the view that a question from the subagent is waiting in the main conversation", async () => {
    // @decision(DL-020)
    stub.withConversations(subagentConversations);
    const page = await open(`&agent=${Child}`);
    for (const ev of subagentLog.filter((e) => e.pos <= 18)) stub.say(ev);
    stub.say({
      name: "permission.asked",
      source: "ephemeral",
      session: Child,
      ask: { id: "per_c", kind: "permission", action: "edit", resources: ["internal/client/retry.go"], call: "call_x" },
      at: "2026-11-08T19:17:48Z",
      pos: 19,
    });
    await settle();
    const waiting = find(page, "subagent-waiting") as HTMLAnchorElement;
    expect(waiting.textContent).toContain("1 waiting on you, in the main conversation");
    expect(waiting.getAttribute("href")).toBe("/session/?id=ses_1");
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

describe("a long transcript", () => {
  // @decision(DL-070)
  it("draws only its newest window and offers the rest on request", async () => {
    const page = await open();
    for (let i = 0; i < 250; i++) {
      stub.say({
        name: "session.text.ended",
        source: "opencode",
        text: `line ${i} said`,
        at: new Date(Date.UTC(2026, 8, 16, 10, 0, i)).toISOString(),
        pos: i + 1,
      });
    }
    await settle();
    const drawn = () => (page.said().match(/line \d+ said/g) ?? []).length;
    expect(drawn()).toBe(200);
    expect(page.said()).not.toContain("line 49 said");
    expect(page.said()).toContain("line 50 said");
    const earlier = page.control("transcript-earlier");
    expect(earlier?.textContent).toContain("Show earlier (50 more)");
    earlier!.click();
    await settle();
    expect(drawn()).toBe(250);
    expect(page.control("transcript-earlier")).toBeNull();
    page.done();
  });
});

// A stopped session the server says can resume offers Resume, worded by
// what comes back; resuming reopens the log where the page left off.
//
// @decision(DL-090)
describe("resuming a stopped session", () => {
  it("offers Resume and follows the resumed session", async () => {
    stub.sessionState("stopped");
    stub.withResume({ mode: "snapshot", until: "2026-11-13T09:00:00Z" });
    const target = document.createElement("div");
    document.body.append(target);
    history.replaceState(null, "", "/session/?id=ses_1");
    const c = mount(SessionDetail, { target });
    await settle();
    stub.endTheStream();
    await settle();
    const opener = target.querySelector('[data-testid="resume-session"]') as HTMLButtonElement | null;
    expect(opener, "no Resume offered").not.toBeNull();
    opener!.click();
    await settle();
    expect(target.textContent).toContain("uncommitted changes included");
    const opens = stub.logOpens();
    (target.querySelector('[data-testid="resume-session-yes"]') as HTMLButtonElement).click();
    await settle();
    await settle();
    expect(stub.posted()).toContain("/api/v1/sessions/ses_1/resume");
    expect(stub.logOpens(), "the log was not reopened").toBeGreaterThan(opens);
    expect(target.querySelector('[data-testid="resume-session"]')).toBeNull();
    unmount(c);
    target.remove();
  });
});

// Below `lg` the sidebar is a sheet opened from a slim title bar, and a
// sent message always brings the page back to the end. CSS decides what is
// drawn and jsdom has none, so these assert the state the markup carries
// and where the page was told to scroll.
//
// @decision(DL-106) @decision(DL-057)
describe("the session page on a phone", () => {
  it("opens the details from the title bar and closes them with Escape", async () => {
    const page = await open();
    const opener = page.control("session-details-open")!;
    const sheet = page.target.querySelector("#session-details")!;
    expect(opener.getAttribute("aria-expanded")).toBe("false");
    expect(sheet.className).not.toContain("max-lg:fixed");

    opener.click();
    flushSync();
    expect(opener.getAttribute("aria-expanded")).toBe("true");
    expect(sheet.className).toContain("max-lg:fixed");

    window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    flushSync();
    expect(opener.getAttribute("aria-expanded")).toBe("false");
    expect(sheet.className).not.toContain("max-lg:fixed");
    page.done();
  });

  it("leads the bar with the repository when the session has one", async () => {
    stub.setSession({ repos: [{ platform: "github", owner: "acme", name: "storefront" }] });
    const page = await open();
    const bar = page.control("session-bar")!;
    expect(bar.textContent).toContain("acme/storefront");
    expect(bar.textContent).toContain("live");
    page.done();
  });

  it("returns to the end when a message is sent from further up the page", async () => {
    const page = await open();
    // A page a long way from its end: the latch is released by a move up.
    let top = 0;
    const el = document.documentElement;
    Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => 5000 });
    Object.defineProperty(el, "clientHeight", { configurable: true, get: () => 700 });
    Object.defineProperty(el, "scrollTop", { configurable: true, get: () => top });
    try {
      top = 3000;
      window.dispatchEvent(new Event("scroll"));
      top = 100;
      window.dispatchEvent(new Event("scroll"));
      await settle();

      const scrolled = vi.spyOn(Element.prototype, "scrollTo");
      page.box()!.value = "one more thing";
      page.box()!.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      page.send().click();
      await settle();
      await new Promise((r) => setTimeout(r, 50));
      expect(scrolled).toHaveBeenCalledWith({ top: 5000, behavior: "auto" });
      scrolled.mockRestore();
    } finally {
      for (const key of ["scrollHeight", "clientHeight", "scrollTop"]) delete (el as unknown as Record<string, unknown>)[key];
      page.done();
    }
  });
});

// @decision(DL-116)
describe("the session page's cost", () => {
  it("shows the sandbox cost only inside the collapsed Sandbox disclosure", async () => {
    stub.setSession({ estimated_cost: 4.82 });
    const page = await open();

    expect(page.said()).not.toContain("Cost so far");
    const sandbox = page.target.querySelector('[data-testid="sandbox-details"]');
    expect(sandbox, "the Sandbox disclosure is missing").not.toBeNull();
    expect((sandbox as HTMLDetailsElement).open).toBe(false);
    expect(sandbox!.textContent).toContain("Sandbox cost");
    expect(sandbox!.textContent).toContain("at least $4.82");
    expect(sandbox!.textContent).toContain("does not include model tokens");
    // The figure is drawn once, and only inside the disclosure.
    expect(page.said().split("at least $4.82")).toHaveLength(2);
    page.done();
  });
});
