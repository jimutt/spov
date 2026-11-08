import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The composer, mounted: what is left after steer.ts holds the logic is
 * markup-only — which control is drawn for which job state, and whether
 * a keystroke is wired to the send.
 *
 * @decision(DL-059) @decision(DL-052)
 */

/**
 * Installed by vi.hoisted: openapi-fetch binds `globalThis.fetch` when
 * the client is CREATED and `client.ts` creates it at import, so a stub
 * installed in the test body is captured by nothing and the request
 * goes to the real network.
 */
const stub = vi.hoisted(() => {
  let release!: (r: Response) => void;
  // openapi-fetch hands `fetch` a Request object and no init, so the
  // body is read off the Request rather than out of the call arguments.
  // A helper that looked at `init.body` sees nothing and every
  // assertion about what was SENT passes vacuously.
  const bodies: Record<string, unknown>[] = [];
  const urls: string[] = [];
  let breaks = false;
  // Only a MESSAGE hangs. Stop and Resume answer at once, because what
  // this file asserts about them is what they said and where they went
  // rather than what the page looks like mid-request.
  let stopped = true;
  let resumeStatus = 204;
  const json = (body: unknown, status: number) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": status === 204 ? "text/plain" : "application/json" },
    });
  const sent = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    if (req.method !== "POST") return new Response(null, { status: 404 });
    const path = new URL(req.url).pathname;
    urls.push(path);
    const raw = await req.text();
    bodies.push((raw ? JSON.parse(raw) : {}) as Record<string, unknown>);
    if (breaks) throw new TypeError("Failed to fetch");
    if (path.endsWith("/interrupt")) {
      return json({ interrupted: stopped, conversation: "ses_1" }, 200);
    }
    if (path.endsWith("/resume")) {
      if (resumeStatus === 204) return new Response(null, { status: 204 });
      return new Response(JSON.stringify({ title: "the job is running, so it cannot be resumed" }), {
        status: resumeStatus,
        headers: { "content-type": "application/problem+json" },
      });
    }
    // Never answers until the test says so, which is what makes "in
    // flight" a state an assertion can hold.
    return new Promise<Response>((resolve) => {
      release = resolve;
    });
  });
  globalThis.fetch = sent as unknown as typeof globalThis.fetch;
  return {
    sent,
    bodies: () => bodies,
    // Module-scoped, so it has to be cleared between tests or every
    // count is the whole file's rather than this test's.
    urls: () => urls,
    reset: () => {
      breaks = false;
      stopped = true;
      resumeStatus = 204;
      bodies.splice(0, bodies.length);
      urls.splice(0, urls.length);
    },
    /** The request never arrives at all, which is not an ApiError. */
    breakTheNetwork: () => (breaks = true),
    mendTheNetwork: () => (breaks = false),
    /** What the sandbox answers to an interrupt. */
    stopAnswers: (was: boolean) => (stopped = was),
    refuseTheResume: (status: number) => (resumeStatus = status),
    answer: () =>
      release(
        new Response(JSON.stringify({ id: "msg_1", conversation: "ses_1", delivery: "steer" }), {
          status: 202,
          headers: { "content-type": "application/json" },
        }),
      ),
  };
});

// Imported AFTER the stub is installed, which is what the hoisting is
// for.
import Composer from "./Composer.svelte";
import type { JobState } from "../lib/steer";

beforeEach(() => stub.reset());

async function settle() {
  for (let i = 0; i < 30; i++) await Promise.resolve();
  flushSync();
}

interface Drawn {
  jobKnown?: boolean;
  /** `null` means "pass none", which `undefined` cannot say here: a
   *  job's id is synthesised for every state so the ordinary tests need
   *  not name one, and without this the `no id` branch is unreachable
   *  from the harness and could only be asserted about. */
  jobId?: string | null;
  /** Whether a turn is running, which is what Stop turns on. */
  inFlight?: boolean;
  /** Supplied when a test needs to control WHEN the re-read lands. */
  onJobChanged?: () => void | Promise<void>;
  /** The session's model, which the composer's chip names. */
  model?: string;
}

function draw(jobState?: JobState, opts: Drawn = {}) {
  const target = document.createElement("div");
  document.body.append(target);
  const track = { open: vi.fn(), sent: vi.fn(), failed: vi.fn() };
  const changed = vi.fn(opts.onJobChanged);
  const given = opts.jobId === undefined ? (jobState ? "job_1" : undefined) : opts.jobId;
  const app = mount(Composer, {
    target,
    props: {
      ref: "ses_1",
      jobKnown: opts.jobKnown ?? true,
      jobState,
      jobId: given ?? undefined,
      inFlight: opts.inFlight ?? false,
      model: opts.model,
      onEcho: track,
      onJobChanged: changed,
    },
  });
  const pick = <T extends Element>(id: string) =>
    target.querySelector(`[data-testid="${id}"]`) as T | null;
  const opened = (toggle: string, note: string) => {
    if (!pick(note)) {
      pick(toggle)?.querySelector("button")?.click();
      flushSync();
    }
    return pick(note)?.textContent ?? "";
  };
  return {
    target,
    track,
    changed,
    box: () => target.querySelector("#composer-text") as HTMLTextAreaElement | null,
    button: () => pick<HTMLButtonElement>("composer-send"),
    after: () => pick<HTMLSelectElement>("composer-after"),
    interruptSend: () => pick<HTMLButtonElement>("composer-interrupt-send"),
    stop: () => pick<HTMLButtonElement>("composer-stop"),
    stopped: () => pick("composer-stopped")?.textContent ?? "",
    resume: () => pick<HTMLButtonElement>("composer-resume"),
    // Both explanations sit behind an (i); these open it first.
    resumeNote: () => opened("composer-controls-info", "composer-resume-note"),
    note: () => opened("composer-note-info", "composer-note"),
    refusal: () => pick("composer-refusal")?.textContent ?? "",
    chip: () => pick<HTMLButtonElement>("composer-options-toggle")!,
    options: () => target.querySelector("#composer-options") as HTMLElement,
    form: () => pick<HTMLFormElement>("composer") as HTMLFormElement,
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("which control the composer draws", () => {
  // A job parked on a question draws the reason instead of a box.
  //
  // @decision(DL-059) @decision(DL-052)
  it("draws the reason instead of a box on a job that cannot take a message", () => {
    const c = draw("blocked");
    expect(c.box()).toBeNull();
    expect(c.refusal()).toContain("parked on a question");
    c.done();
  });

  it("draws the box on a running job", () => {
    const c = draw("running");
    expect(c.box()).not.toBeNull();
    c.done();
  });

  // @decision(DL-052)
  it("draws the box on a job already steered", () => {
    const c = draw("steered");
    expect(c.box()).not.toBeNull();
    c.done();
  });

  // What follows a message is asked on a running job only.
  //
  // @decision(DL-059) @decision(DL-052)
  it("asks what follows only on a running job", () => {
    const job = draw("running");
    expect(job.after()).not.toBeNull();
    job.done();

    for (const state of [undefined, "steered"] as const) {
      const other = draw(state);
      expect(other.after(), String(state)).toBeNull();
      other.done();
    }
  });

  // The note explains the after choice, beside it; Send has no line of
  // its own on any screen.
  //
  // @decision(DL-059) @decision(DL-117)
  it("draws the note beside the after choice only, and no line under Send", () => {
    const job = draw("running");
    expect(job.note()).toContain("carries on by itself");
    expect(job.target.textContent).not.toContain("Read once the current step finishes");
    job.done();

    for (const state of [undefined, "steered"] as const) {
      const other = draw(state);
      expect(other.target.querySelector('[data-testid="composer-note-info"]'), String(state)).toBeNull();
      expect(other.target.textContent, String(state)).not.toContain("Read once the current step finishes");
      other.done();
    }
  });
});

describe("sending from the box", () => {
  // Enter sends, Shift-Enter does not — wired in the template, so only
  // a mounted component can check it. Does not assert that Shift-Enter
  // inserts a newline: jsdom does no default text editing, so only the
  // absence of a send is observable here.
  //
  // @decision(DL-059)
  it("sends on Enter and not on Shift-Enter", async () => {
    const c = draw("running");
    const box = c.box()!;
    box.value = "carry on";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();

    const shifted = new KeyboardEvent("keydown", {
      key: "Enter",
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    box.dispatchEvent(shifted);
    await settle();
    expect(stub.bodies()).toHaveLength(0);
    expect(shifted.defaultPrevented).toBe(false);

    const plain = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    box.dispatchEvent(plain);
    await settle();
    expect(plain.defaultPrevented).toBe(true);
    expect(stub.bodies()).toHaveLength(1);
    expect(stub.bodies()[0].text).toBe("carry on");
    expect(String(stub.bodies()[0].id)).toMatch(/^msg_/);

    stub.answer();
    await settle();
    c.done();
  });

  // Where the message went, which nothing else in this suite says. A
  // send posted at the wrong `ref` reaches a session the reader is not
  // looking at and can be one they do not own — the server refuses
  // that, but the page must not be the thing asking.
  //
  // @decision(DL-059) @decision(DL-019)
  it("posts to the session it was given, and clears the box on success", async () => {
    const c = draw("running");
    const box = c.box()!;
    box.value = "hello";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    expect(stub.urls()).toEqual(["/api/v1/sessions/ses_1/messages"]);

    stub.answer();
    await settle();
    // Cleared, or the next Enter sends the same words again.
    expect(c.box()!.value).toBe("");
    // And the reader can carry on typing without reaching for the
    // mouse: the box is disabled while a send is in flight, and a
    // disabled element loses focus.
    expect(document.activeElement).toBe(c.box());
    c.done();
  });

  // The echo opens before the answer arrives, `sent` only once it does.
  //
  // @decision(DL-059)
  it("reports the echo before the server answers", async () => {
    const c = draw("running");
    const box = c.box()!;
    box.value = "hello";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();

    expect(c.track.open).toHaveBeenCalledTimes(1);
    expect(c.track.sent).not.toHaveBeenCalled();
    stub.answer();
    await settle();
    expect(c.track.sent).toHaveBeenCalledTimes(1);
    c.done();
  });

  // A disabled button dispatches nothing, so the submit is fired
  // directly on the form to check the handler's own guard.
  //
  // @decision(DL-059) @decision(DL-019)
  it("refuses a second send while one is in flight", async () => {
    const c = draw("running");
    const box = c.box()!;
    box.value = "hello";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    expect(stub.bodies()).toHaveLength(1);
    expect(c.button()!.disabled).toBe(true);

    c.form().dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await settle();
    expect(stub.bodies()).toHaveLength(1);

    stub.answer();
    await settle();
    c.done();
  });

  // Send steers, with the reader's choice of what follows, and the
  // choice goes back to continue after the send.
  //
  // @decision(DL-059) @decision(DL-019) @decision(DL-052)
  it("sends the message with what the reader chose to follow it", async () => {
    const c = draw("running");
    c.after()!.value = "handover";
    c.after()!.dispatchEvent(new Event("change", { bubbles: true }));
    const box = c.box()!;
    box.value = "over to me after this";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    expect(c.note()).toContain("pauses for you");

    c.button()!.click();
    await settle();
    expect(stub.bodies()[0].delivery).toBe("steer");
    expect(stub.bodies()[0].after).toBe("handover");

    stub.answer();
    await settle();
    expect(c.after()!.value).toBe("continue");
    c.done();
  });

  // There is no combined Interrupt & send: Send, then Interrupt, does the same.
  //
  // @decision(DL-059)
  it("draws no Interrupt & send", () => {
    const c = draw("running", { inFlight: true });
    expect(c.interruptSend()).toBeNull();
    c.done();
  });

  // A transport failure must reach the reader, with the text given back
  // and the button offered again.
  //
  // @decision(DL-059)
  it("shows a transport failure and gives the words back", async () => {
    stub.breakTheNetwork();
    const c = draw("running");
    const box = c.box()!;
    box.value = "still there?";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();

    expect(c.track.failed).toHaveBeenCalledTimes(1);
    expect(c.target.querySelector('[role="alert"]')?.textContent).toContain("Failed to fetch");
    expect(c.box()!.value).toBe("still there?");
    expect(c.button()!.disabled).toBe(false);
    c.done();
  });

  // A retry with another choice of what follows is another message: it
  // mints its own id rather than replaying the one the failed send minted.
  //
  // @decision(DL-059)
  it("mints a new id when the retry chooses differently", async () => {
    stub.breakTheNetwork();
    const c = draw("running");
    const box = c.box()!;
    box.value = "use the other branch";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    stub.mendTheNetwork();
    c.after()!.value = "handover";
    c.after()!.dispatchEvent(new Event("change", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    const [first, second] = stub.bodies();
    expect(first.after).toBe("continue");
    expect(second.after).toBe("handover");
    expect(second.id).not.toBe(first.id);
    stub.answer();
    await settle();
    c.done();
  });

  // A retry after an indeterminate failure carries the same id, never a
  // fresh one, so it cannot deliver the message twice.
  //
  // @decision(DL-059)
  it("sends the same id again after a failure that may have arrived", async () => {
    stub.breakTheNetwork();
    const c = draw("running");
    const box = c.box()!;
    box.value = "are you there?";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    const first = String(stub.bodies()[0].id);

    stub.mendTheNetwork();
    c.button()!.click();
    await settle();
    expect(stub.bodies()).toHaveLength(2);
    expect(stub.bodies()[1].id).toBe(first);

    stub.answer();
    await settle();
    // And once it lands, the NEXT message is a new one.
    box.value = "again";
    box.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    c.button()!.click();
    await settle();
    expect(stub.bodies()[2].id).not.toBe(first);
    stub.answer();
    await settle();
    c.done();
  });

  // An empty box sends nothing, by the handler as well as by the
  // attribute — Enter reaches the handler directly.
  it("sends nothing from an empty box", async () => {
    const c = draw("running");
    const before = stub.bodies().length;
    c.box()!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await settle();
    expect(stub.bodies()).toHaveLength(before);
    expect(c.button()!.disabled).toBe(true);
    c.done();
  });
});

describe("stopping a turn", () => {
  // Stop is drawn only while a turn is running.
  //
  // @decision(DL-059) @decision(DL-053)
  it("draws Stop only while a turn is running", () => {
    const idle = draw("running", { inFlight: false });
    expect(idle.stop()).toBeNull();
    idle.done();

    const busy = draw("running", { inFlight: true });
    expect(busy.stop()).not.toBeNull();
    busy.done();
  });

  // @decision(DL-059) @decision(DL-053)
  it("draws no Stop on a job whose turn must not be stopped", () => {
    for (const state of ["blocked", "queued", "provisioning", "delivering"] as JobState[]) {
      const c = draw(state, { inFlight: true });
      expect(c.stop(), state).toBeNull();
      c.done();
    }
  });

  // An interactive session has no job and none of DL-052's or DL-053's
  // job rules apply to it.
  it("draws Stop on an interactive session", () => {
    const c = draw(undefined, { inFlight: true });
    expect(c.stop()).not.toBeNull();
    c.done();
  });

  // @decision(DL-059) @decision(DL-019) @decision(DL-053)
  it("posts the interrupt to the session it was given, naming no conversation", async () => {
    const c = draw("running", { inFlight: true });
    c.stop()!.click();
    await settle();
    expect(stub.urls()).toEqual(["/api/v1/sessions/ses_1/interrupt"]);
    expect(stub.bodies()[0]).toEqual({});
    expect(c.stopped()).toContain("The turn was interrupted");
    // Does not re-read the job; see DL-059.
    expect(c.changed).not.toHaveBeenCalled();
    c.done();
  });

  // The interrupted turn's end and the resumed turn's start can land
  // together, so `inFlight` never falls; the outcome still goes once
  // the resumed turn has ended.
  //
  // @decision(DL-059)
  it("drops the outcome once a later turn has ended, even if in-flight never fell", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const props = $state({
      ref: "ses_1",
      jobKnown: true,
      inFlight: true,
      turnsEnded: 3,
      onEcho: { open: vi.fn(), sent: vi.fn(), failed: vi.fn() },
    });
    const app = mount(Composer, { target, props });
    const said = () => target.querySelector('[data-testid="composer-stopped"]')?.textContent ?? "";
    (target.querySelector('[data-testid="composer-stop"]') as HTMLButtonElement).click();
    await settle();
    expect(said()).toContain("The turn was interrupted");
    // The interrupted turn ends, and the turn reading what was sent has
    // already begun: still the outcome of the last turn to end.
    props.turnsEnded = 4;
    flushSync();
    expect(said()).toContain("The turn was interrupted");
    props.turnsEnded = 5;
    props.inFlight = false;
    flushSync();
    expect(said()).toBe("");
    unmount(app);
    target.remove();
  });

  // `interrupted: false` is an ordinary success and must not read as
  // "the session was idle".
  //
  // @decision(DL-059) @decision(DL-053)
  it("reports a false as success without claiming the session was idle", async () => {
    stub.stopAnswers(false);
    const c = draw("running", { inFlight: true });
    c.stop()!.click();
    await settle();
    expect(c.target.querySelector('[role="alert"]')).toBeNull();
    const said = c.stopped().toLowerCase();
    expect(said).toContain("accepted");
    expect(said).toContain("either");
    c.done();
  });

  // @decision(DL-059)
  it("shows a stop that failed", async () => {
    stub.breakTheNetwork();
    const c = draw("running", { inFlight: true });
    c.stop()!.click();
    await settle();
    expect(c.target.querySelector('[role="alert"]')?.textContent).toContain("Failed to fetch");
    expect(c.stopped()).toBe("");
    c.done();
  });

  // The note may name Interrupt only while it is drawn.
  //
  // @decision(DL-059)
  it("names Interrupt in the note only while it is drawn", () => {
    const idle = draw("running", { inFlight: false });
    expect(idle.note().toLowerCase()).not.toContain("interrupt");
    idle.done();

    const busy = draw("running", { inFlight: true });
    expect(busy.note()).toContain("Interrupt stops that step");
    busy.done();
  });
});

describe("the status bar", () => {
  // One row holds the state and every control, not a row per control.
  //
  // @decision(DL-059)
  it("puts Interrupt and Resume in one bar under one state", () => {
    const c = draw("steered", { inFlight: true });
    const bar = c.target.querySelector('[data-testid="composer-status"]')!;
    expect(bar.textContent).toContain("Working on a step · paused for you");
    expect(bar.querySelector('[data-testid="composer-stop"]')).not.toBeNull();
    expect(bar.querySelector('[data-testid="composer-resume"]')).not.toBeNull();
    // The explanations stay closed until asked for.
    expect(c.target.querySelector('[data-testid="composer-resume-note"]')).toBeNull();
    expect(c.resumeNote()).toContain("Interrupt: Stops the step");
    expect(c.resumeNote()).toContain("Resume job: The job carries on");
    c.done();
  });

  it("is not drawn on a running job between steps", () => {
    const c = draw("running", { inFlight: false });
    expect(c.target.querySelector('[data-testid="composer-status"]')).toBeNull();
    c.done();
  });
});

describe("resuming a job", () => {
  // @decision(DL-059) @decision(DL-054)
  it("offers Resume on a steered job and on no other running one", () => {
    const steered = draw("steered");
    expect(steered.resume()).not.toBeNull();
    steered.done();

    const running = draw("running");
    expect(running.resume()).toBeNull();
    running.done();

    const interactive = draw(undefined);
    expect(interactive.resume()).toBeNull();
    interactive.done();
  });

  // The state Resume matters most in, and the one a nested control
  // would have missed: the send is REFUSED on a blocked job, so a
  // Resume inside the form would be absent exactly here.
  //
  // @decision(DL-059) @decision(DL-054)
  it("offers Resume on a blocked job, where the box is refused", () => {
    const c = draw("blocked");
    expect(c.box()).toBeNull();
    expect(c.refusal()).toContain("parked on a question");
    expect(c.resume()).not.toBeNull();
    expect(c.resumeNote().toLowerCase()).toContain("answer it in the transcript");
    c.done();
  });

  // @decision(DL-059) @decision(DL-054)
  it("posts to the job's own endpoint", async () => {
    const c = draw("steered", { jobId: "job_9" });
    c.resume()!.click();
    await settle();
    expect(stub.urls()).toEqual(["/api/v1/jobs/job_9/resume"]);
    expect(c.changed).toHaveBeenCalled();
    c.done();
  });

  // Stays disabled until the page has read that the job moved, not
  // merely until the 204 lands.
  //
  // @decision(DL-059) @decision(DL-054)
  it("holds the button until the page has read that the job moved", async () => {
    let readTheRow!: () => void;
    const c = draw("steered", {
      onJobChanged: () => new Promise<void>((resolve) => (readTheRow = resolve)),
    });
    c.resume()!.click();
    await settle();
    expect(stub.urls()).toHaveLength(1);
    expect(c.resume()!.disabled, "re-enabled before the page knew").toBe(true);

    // A second press in that window is the one that would 409.
    c.resume()!.click();
    await settle();
    expect(stub.urls()).toHaveLength(1);

    readTheRow();
    await settle();
    expect(c.resume()!.disabled).toBe(false);
    c.done();
  });

  // @decision(DL-059)
  it("draws no Resume without a job to name", () => {
    const c = draw("steered", { jobId: null });
    expect(c.resume()).toBeNull();
    c.done();
  });

  // @decision(DL-059) @decision(DL-054)
  it("shows a refused resume and offers the button again", async () => {
    stub.refuseTheResume(409);
    const c = draw("steered");
    c.resume()!.click();
    await settle();
    expect(c.target.querySelector('[role="alert"]')?.textContent).toContain("cannot be resumed");
    expect(c.resume()!.disabled).toBe(false);
    c.done();
  });
});

// Below `md` the model and the choice of what follows are behind one chip.
// CSS decides what is drawn, and jsdom has none, so these assert the chip's
// words and the state it toggles.
//
// @decision(DL-106) @decision(DL-014) @decision(DL-059)
describe("the options a phone folds away", () => {
  it("names the model on the chip and opens the options on a press", () => {
    const c = draw("running", { model: "anthropic/claude-sonnet-4-5#high" });
    expect(c.chip().textContent).toContain("claude-sonnet-4-5");
    expect(c.chip().textContent).not.toContain("anthropic/");
    expect(c.chip().getAttribute("aria-expanded")).toBe("false");
    expect(c.options().className).toContain("hidden");

    c.chip().click();
    flushSync();
    expect(c.chip().getAttribute("aria-expanded")).toBe("true");
    expect(c.options().className).not.toContain("hidden");
    expect(c.options().contains(c.after())).toBe(true);
    c.done();
  });

  it("says on the chip that the next message hands the job over", () => {
    const c = draw("running", { model: "anthropic/claude-sonnet-4-5" });
    expect(c.chip().textContent).not.toContain("hand over");
    c.after()!.value = "handover";
    c.after()!.dispatchEvent(new Event("change", { bubbles: true }));
    flushSync();
    expect(c.chip().textContent).toContain("hand over");
    c.done();
  });

  it("falls back to a plain word when no model is known", () => {
    const c = draw("running");
    expect(c.chip().textContent).toContain("Options");
    c.done();
  });
});

// Below `md` the form is one container whose toolbar (the chip and Send)
// is folded away while the box is empty, unfocused and the options shut.
// jsdom has no layout, so this asserts the state the stylesheet reads,
// `data-toolbar`, and which controls are where.
//
// @decision(DL-117) @decision(DL-106) @decision(DL-059)
describe("the phone toolbar", () => {
  function type(c: ReturnType<typeof draw>, words: string) {
    c.box()!.value = words;
    c.box()!.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
  }
  const state = (c: ReturnType<typeof draw>) => c.form().getAttribute("data-toolbar");

  it("is folded while the box is empty, unfocused and the options are shut", () => {
    const c = draw("running");
    expect(state(c)).toBe("folded");
    expect(c.chip().classList.contains("hidden")).toBe(true);
    expect(c.button()!.disabled).toBe(true);
    c.done();
  });

  it("opens on focus and folds again when focus leaves the form", () => {
    const c = draw("running");
    c.box()!.focus();
    flushSync();
    expect(state(c)).toBe("open");
    expect(c.chip().classList.contains("hidden")).toBe(false);

    c.box()!.blur();
    flushSync();
    expect(state(c)).toBe("folded");
    c.done();
  });

  it("stays open while focus moves between the form's own controls", () => {
    const c = draw("running", { model: "anthropic/claude-sonnet-4-5" });
    c.box()!.focus();
    c.chip().focus();
    flushSync();
    expect(state(c)).toBe("open");
    c.done();
  });

  it("stays open for as long as there are words, and folds when they go", () => {
    const c = draw("running");
    type(c, "half a thought");
    expect(state(c)).toBe("open");
    expect(c.button()!.disabled).toBe(false);
    c.box()!.blur();
    flushSync();
    expect(state(c)).toBe("open");
    type(c, "");
    expect(state(c)).toBe("folded");
    expect(c.button()!.disabled).toBe(true);
    c.done();
  });

  it("never folds while the options are open", () => {
    const c = draw("running", { model: "anthropic/claude-sonnet-4-5" });
    c.box()!.focus();
    c.chip().click();
    flushSync();
    expect(c.chip().getAttribute("aria-expanded")).toBe("true");
    c.box()!.blur();
    flushSync();
    expect(state(c)).toBe("open");

    c.chip().click();
    flushSync();
    expect(c.chip().getAttribute("aria-expanded")).toBe("false");
    expect(state(c)).toBe("folded");
    c.done();
  });

  it("keeps the box focused on a press of the chip or Send", () => {
    const c = draw("running");
    for (const el of [c.chip(), c.button()!]) {
      const press = new MouseEvent("mousedown", { bubbles: true, cancelable: true });
      el.dispatchEvent(press);
      expect(press.defaultPrevented).toBe(true);
    }
    c.done();
  });

  it("draws each control once, with Send and the chip inside the form", () => {
    const c = draw("running", { model: "anthropic/claude-sonnet-4-5" });
    expect(c.target.querySelectorAll('[data-testid="composer-send"]')).toHaveLength(1);
    expect(c.target.querySelectorAll('[data-testid="composer-options-toggle"]')).toHaveLength(1);
    expect(c.target.querySelectorAll("#composer-options")).toHaveLength(1);
    expect(c.form().contains(c.chip())).toBe(true);
    expect(c.form().contains(c.button())).toBe(true);
    expect(c.form().contains(c.options())).toBe(true);
    c.done();
  });

  it("keeps Interrupt and Resume outside the form", () => {
    const running = draw("running", { inFlight: true });
    expect(running.stop()).not.toBeNull();
    expect(running.form().contains(running.stop())).toBe(false);
    running.done();

    const steered = draw("steered");
    expect(steered.resume()).not.toBeNull();
    expect(steered.form().contains(steered.resume())).toBe(false);
    steered.done();
  });
});

// Enter on a touch screen writes a new line and the button sends. The
// pointer is what decides, so the stub answers only the coarse query.
//
// @decision(DL-106) @decision(DL-059)
describe("Enter on a touch screen", () => {
  const real = window.matchMedia;
  const coarse = (on: boolean) => {
    window.matchMedia = ((q: string) => ({ matches: on && q.includes("pointer: coarse") })) as typeof window.matchMedia;
  };
  afterEach(() => {
    window.matchMedia = real;
  });

  function typed(c: ReturnType<typeof draw>, words: string) {
    c.box()!.value = words;
    c.box()!.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
  }

  it("leaves Enter to write a new line, and sends from the button", async () => {
    coarse(true);
    const c = draw("running");
    typed(c, "first line");
    const enter = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    c.box()!.dispatchEvent(enter);
    await settle();
    expect(enter.defaultPrevented).toBe(false);
    expect(stub.bodies()).toHaveLength(0);
    expect(c.box()!.getAttribute("enterkeyhint")).toBe("enter");

    c.button()!.click();
    await settle();
    expect(stub.bodies()).toHaveLength(1);
    stub.answer();
    await settle();
    c.done();
  });

  it("still sends on Ctrl-Enter, for a tablet with a keyboard", async () => {
    coarse(true);
    const c = draw("running");
    typed(c, "from a keyboard");
    c.box()!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", ctrlKey: true, bubbles: true, cancelable: true }));
    await settle();
    expect(stub.bodies()).toHaveLength(1);
    stub.answer();
    await settle();
    c.done();
  });

  it("says Enter sends only where Enter does", () => {
    coarse(false);
    const keyboard = draw("running");
    expect(keyboard.target.textContent).toContain("to send");
    expect(keyboard.box()!.getAttribute("enterkeyhint")).toBe("send");
    keyboard.done();

    coarse(true);
    const touch = draw("running");
    expect(touch.target.textContent).not.toContain("to send");
    touch.done();
  });
});
