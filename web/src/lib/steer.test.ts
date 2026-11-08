import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api/client";
import {
  type After,
  type Echo,
  type SteerRequest,
  KNOWN_JOB_STATES,
  MessageIDPrefix,
  TurnEnders,
  TurnStarted,
  canStop,
  cancel,
  composerStatus,
  controlsNote,
  inboxClosed,
  queuedNote,
  composerShown,
  delivered,
  deliveryNote,
  draftProblem,
  echoNote,
  everyJobStateIsKnown,
  Failed,
  newMessageID,
  offersAfter,
  resumable,
  resumeNote,
  send,
  sendability,
  sendsOnEnter,
  stop,
  stopOutcome,
  toSteerRequest,
  turnInFlight,
  turnsEnded,
  unseen,
} from "./steer";
import { childIds, type Activity, type Conversation } from "./timeline";
import { currentModel } from "./modelpick";
import { Child, subagentConversations, subagentLog } from "./subagent.fixture";

/**
 * The composer's rules. Everything a reader is told about what a
 * message does, and everything that decides whether their own words are
 * on screen, lives in steer.ts precisely so that it can be asserted.
 *
 * @decision(DL-059)
 */

/** A minimal log frame, as the live stream delivers one. */
function frame(over: Partial<Activity>): Activity {
  return {
    name: "session.inbox.enqueued",
    source: "opencode",
    at: "2026-09-16T10:00:00Z",
    pos: 1,
    ...over,
  } as Activity;
}

function conv(messages: Conversation["messages"]): Conversation[] {
  return [{ id: "ses_1", messages }];
}

/** A tracker that records the order of the three callbacks. */
function tracker() {
  const steps: string[] = [];
  const echoes: Echo[] = [];
  return {
    steps,
    echoes,
    track: {
      open: (e: Echo) => {
        steps.push(`open:${e.key}`);
        echoes.push(e);
      },
      sent: (key: string, id: string) => {
        steps.push(`sent:${key}:${id}`);
        const e = echoes.find((x) => x.key === key);
        if (e) {
          e.id = id;
          e.sending = false;
        }
      },
      failed: (key: string) => {
        steps.push(`failed:${key}`);
        const at = echoes.findIndex((x) => x.key === key);
        if (at >= 0) echoes.splice(at, 1);
      },
    },
  };
}

/**
 * A post that answers `body` and records what it was sent.
 *
 * A refusal lands in `error` rather than `data`, which is where
 * openapi-fetch puts a non-2xx body — a helper that put it in `data`
 * would leave every assertion about the server's own words passing on
 * an ApiError carrying none.
 */
function answers(body: unknown, status = 202) {
  const seen: SteerRequest[] = [];
  const ok = status < 400;
  const post = vi.fn(async (sent: SteerRequest) => {
    seen.push(sent);
    return {
      response: { ok, status },
      data: ok ? body : undefined,
      error: ok ? undefined : body,
    } as never;
  });
  return { post, seen };
}

describe("the message id", () => {
  // Pins the id shape the server checks (prefix, length, character set).
  //
  // @decision(DL-059) @decision(DL-019)
  it("is one the server will accept", () => {
    const id = newMessageID();
    expect(id.startsWith(MessageIDPrefix)).toBe(true);
    expect(id.length).toBeLessThanOrEqual(128);
    expect(id).toMatch(/^[0-9a-zA-Z_-]+$/);
  });

  it("is a different id every time", () => {
    const minted = new Set(Array.from({ length: 500 }, () => newMessageID()));
    expect(minted.size).toBe(500);
  });

  // The id's timestamp half orders it, so anything sorting by id agrees
  // with the clock.
  //
  // @decision(DL-059)
  it("sorts by the moment it was minted", () => {
    const early = newMessageID(1_700_000_000_000, () => 0);
    const late = newMessageID(1_700_000_000_001, () => 0);
    expect(early < late).toBe(true);
    // And still, once the millisecond count needs a twelfth hex digit.
    expect(newMessageID(0xfffffffffff, () => 0) < newMessageID(0x100000000000, () => 0)).toBe(true);
  });

  // A stub — or a future platform — returning exactly 1 would index one
  // past the end of the alphabet and put the literal string "undefined"
  // in the id, which the server refuses on its character set. The
  // failure would be a 422 on every send from that browser.
  it("survives a random source that returns its upper bound", () => {
    const id = newMessageID(1_700_000_000_000, () => 1);
    expect(id).toMatch(/^[0-9a-zA-Z_-]+$/);
    expect(id).not.toContain("undefined");
  });
});

describe("the draft", () => {
  it("needs something other than whitespace", () => {
    expect(draftProblem("   \n ")).not.toBe("");
    expect(draftProblem("hello")).toBe("");
  });

  // The server owns MaxMessageBytes and refuses with its own words. A
  // second copy of the bound here is a second thing to keep in step,
  // and this is the assertion that says the copy was not made.
  //
  // @decision(DL-059) @decision(DL-061)
  it("restates no bound the server owns", () => {
    expect(draftProblem("x".repeat(1_000_000))).toBe("");
  });
});

describe("the keyboard", () => {
  // Enter sends and Shift-Enter starts a line — the OPPOSITE of the job
  // form's rule, because that form provisions a sandbox and this goes
  // to one already running.
  //
  // @decision(DL-059) @decision(DL-061)
  it("sends on Enter and not on Shift-Enter", () => {
    expect(sendsOnEnter({ key: "Enter" })).toBe(true);
    expect(sendsOnEnter({ key: "Enter", shiftKey: true })).toBe(false);
    expect(sendsOnEnter({ key: "a" })).toBe(false);
  });

  // @decision(DL-106) @decision(DL-059)
  it("writes a new line on Enter on a touch screen, and sends on Ctrl or Cmd-Enter", () => {
    expect(sendsOnEnter({ key: "Enter" }, true)).toBe(false);
    expect(sendsOnEnter({ key: "Enter", shiftKey: true }, true)).toBe(false);
    expect(sendsOnEnter({ key: "Enter", ctrlKey: true }, true)).toBe(true);
    expect(sendsOnEnter({ key: "Enter", metaKey: true }, true)).toBe(true);
    expect(sendsOnEnter({ key: "a", ctrlKey: true }, true)).toBe(false);
    // The default is the keyboard's rule.
    expect(sendsOnEnter({ key: "Enter", ctrlKey: true })).toBe(true);
    expect(sendsOnEnter({ key: "Enter", isComposing: true, ctrlKey: true }, true)).toBe(false);
  });

  // An IME commits its candidate with Enter. Without this, accepting a
  // suggestion posts a half-typed word — every time, for anyone typing
  // Japanese, Chinese or Korean.
  it("does not send the Enter that commits an IME candidate", () => {
    expect(sendsOnEnter({ key: "Enter", isComposing: true })).toBe(false);
    expect(sendsOnEnter({ key: "Enter", keyCode: 229 })).toBe(false);
  });
});

describe("whether the composer is drawn", () => {
  // @decision(DL-059) @decision(DL-017)
  it("needs the row to say running AND the stream not to have ended", () => {
    expect(composerShown("running", false)).toBe(true);
    expect(composerShown("running", true)).toBe(false);
    expect(composerShown("stopped", false)).toBe(false);
    expect(composerShown(undefined, false)).toBe(false);
  });
});

describe("what a message means on a job", () => {
  // Pins which states refuse, not the wording; the wording is the
  // page's own.
  //
  // @decision(DL-059) @decision(DL-052)
  it("refuses the states where a message would be enqueued and not delivered", () => {
    for (const state of ["blocked", "queued", "provisioning", "delivering"]) {
      expect(sendability(state).can, state).toBe(false);
      expect(sendability(state).why, state).not.toBe("");
    }
    for (const state of ["succeeded", "failed", "cancelled"]) {
      expect(sendability(state).can, state).toBe(false);
    }
  });

  // @decision(DL-052)
  it("allows a running job and one already steered", () => {
    expect(sendability("running").can).toBe(true);
    expect(sendability("steered").can).toBe(true);
  });

  it("treats a session with no job as interactive", () => {
    expect(sendability(undefined)).toEqual({ can: true, why: "" });
  });

  // @decision(DL-059) @decision(DL-052)
  it("lets an unknown state through for the server to judge", () => {
    expect(sendability("hibernating").can).toBe(true);
  });

  it("has been asked about every state the API document carries", () => {
    // The compile-time half is the proof; `astro check` is what runs
    // it. What the runtime half adds is that a state added here with
    // no reason is caught: a refusal with an empty `why` draws a
    // sentence that says nothing, which is worse than no box at all.
    expect(everyJobStateIsKnown).toBe(true);
    for (const state of KNOWN_JOB_STATES) {
      const { can, why } = sendability(state);
      expect(why !== "", `${state}: a refusal must say why and a pass must not`).toBe(!can);
    }
    expect(KNOWN_JOB_STATES).toContain("steered");
  });
});

describe("what follows a message", () => {
  // @decision(DL-059) @decision(DL-052)
  it("is asked only on a running job", () => {
    expect(offersAfter("running", true)).toBe(true);
    expect(offersAfter(undefined, true)).toBe(false);
    for (const state of KNOWN_JOB_STATES.filter((s) => s !== "running")) {
      expect(offersAfter(state, true), state).toBe(false);
    }
  });

  // @decision(DL-059) @decision(DL-057)
  it("is not asked before the page knows whether there is a job", () => {
    expect(offersAfter("running", false)).toBe(false);
    // And the copy in that window claims nothing about a job either way.
    const note = deliveryNote(undefined, "continue", false);
    expect(note.toLowerCase()).not.toContain("job");
    expect(note).toBe(deliveryNote("running", "handover", false));
  });
});

describe("the copy", () => {
  // @decision(DL-059) @decision(DL-052)
  it("says a message is read after the current step, and never that a turn stops", () => {
    for (const state of [undefined, ...KNOWN_JOB_STATES]) {
      for (const after of ["continue", "handover"] as After[]) {
        const said = deliveryNote(state, after, true).toLowerCase();
        expect(said, `${state}/${after}`).toContain("once its current step finishes");
        expect(said, `${state}/${after}`).not.toContain("interrupt");
      }
    }
  });

  // @decision(DL-059) @decision(DL-052)
  it("says what follows on a job", () => {
    expect(deliveryNote("running", "continue", true)).toContain("carries on by itself");
    expect(deliveryNote("running", "handover", true)).toContain("pauses for you");
    expect(deliveryNote("steered", "continue", true)).toContain("waits for you");
    expect(deliveryNote(undefined, "continue", true).toLowerCase()).not.toContain("job");
  });

  // @decision(DL-059) @decision(DL-053)
  it("names Interrupt once it is on the page", () => {
    const said = deliveryNote("running", "continue", true, true);
    expect(said).toContain("Interrupt stops that step, and the agent reads it at once.");
    // It is an addition, so the constrained half is unchanged.
    expect(said.startsWith(deliveryNote("running", "continue", true))).toBe(true);
  });
});

describe("the request body", () => {
  // @decision(DL-059) @decision(DL-019) @decision(DL-052)
  it("names no conversation and carries the id, the delivery and what follows", () => {
    const body = toSteerRequest("  hello  ", "queue", "msg_x");
    expect(body).toEqual({ text: "hello", id: "msg_x", delivery: "queue", after: "continue" });
    expect(body.conversation).toBeUndefined();
    expect(toSteerRequest("x", "steer", "msg_y", "handover")).toEqual({
      text: "x",
      id: "msg_y",
      delivery: "steer",
      after: "handover",
    });
  });
});

describe("a send", () => {
  // The echo must open before the request, not on its answer, or it is
  // no echo at all.
  //
  // @decision(DL-059)
  it("draws the echo before the request goes out", async () => {
    const t = tracker();
    const order: string[] = [];
    const post = vi.fn(async () => {
      order.push(...t.steps);
      return { response: { ok: true, status: 202 }, data: { id: "msg_k" } } as never;
    });
    await send("hi", "steer", post, t.track, "", () => "msg_k", () => 1000);
    expect(order).toEqual(["open:msg_k"]);
    expect(t.steps).toEqual(["open:msg_k", "sent:msg_k:msg_k"]);
    expect(t.echoes[0]).toEqual({
      key: "msg_k",
      id: "msg_k",
      text: "hi",
      at: 1000,
      sending: false,
    });
  });

  it("sends the id it minted", async () => {
    const { post, seen } = answers({ id: "msg_k", conversation: "ses_1", delivery: "steer" });
    const t = tracker();
    await send("  hi  ", "steer", post, t.track, "", () => "msg_k");
    expect(seen).toEqual([{ text: "hi", id: "msg_k", delivery: "steer", after: "continue" }]);
  });

  // @decision(DL-059) @decision(DL-019)
  it("reconciles on the id the server reports, not the one it minted", async () => {
    const { post } = answers({ id: "msg_theirs", conversation: "ses_1", delivery: "steer" });
    const t = tracker();
    await send("hi", "steer", post, t.track, "", () => "msg_mine");
    expect(t.steps).toEqual(["open:msg_mine", "sent:msg_mine:msg_theirs"]);
    // The key does not move, or the row would be renumbered under a
    // reader mid-send.
    expect(t.echoes[0].key).toBe("msg_mine");
    expect(t.echoes[0].id).toBe("msg_theirs");
  });

  // An answer this build cannot read must not lose an id the caller
  // already holds — it is what was sent, so it is still the best key
  // there is.
  it("keeps its own id when the answer carries none", async () => {
    const { post } = answers({ conversation: "ses_1", delivery: "steer" });
    const t = tracker();
    await send("hi", "steer", post, t.track, "", () => "msg_mine");
    expect(t.echoes[0].id).toBe("msg_mine");
  });

  // A refusal takes the row back out, or the reader sees a message
  // that was never sent sitting in their transcript forever — and it
  // carries the server's own words for the caller to render.
  it("withdraws the echo when the server refuses", async () => {
    const { post } = answers({ title: "the job is blocked" }, 409);
    const t = tracker();
    const err = await send("hi", "steer", post, t.track, "", () => "msg_k").catch((e) => e);
    expect(err).toBeInstanceOf(Failed);
    expect(err.cause).toBeInstanceOf(ApiError);
    expect((err.cause as ApiError).problem?.title).toBe("the job is blocked");
    expect(t.steps).toEqual(["open:msg_k", "failed:msg_k"]);
    expect(t.echoes).toEqual([]);
  });

  // And when the request never arrives at all, which is a different
  // path through the function.
  it("withdraws the echo when the request throws", async () => {
    const boom = new Error("the network went away");
    const t = tracker();
    const err = await send("hi", "steer", async () => {
      throw boom;
    }, t.track, "", () => "msg_k").catch((e) => e);
    expect(err).toBeInstanceOf(Failed);
    expect(err.cause).toBe(boom);
    expect(t.steps).toEqual(["open:msg_k", "failed:msg_k"]);
  });
});

describe("pressing send again", () => {
  // Every 4xx is decided before the message reaches the sandbox, so
  // nothing was delivered and the next send is a fresh message.
  //
  // @decision(DL-059) @decision(DL-019)
  it("mints a new id after a refusal the server computed", async () => {
    const { post } = answers({ title: "the job is blocked" }, 409);
    const t = tracker();
    const err = await send("hi", "steer", post, t.track, "", () => "msg_k").catch((e) => e);
    expect(err.replay).toBe("");
  });

  // A 5xx or a dropped connection is indeterminate, so the id is
  // replayed and the sandbox's own dedupe decides.
  //
  // @decision(DL-059)
  it("replays the same id after a failure that may have arrived", async () => {
    const t = tracker();
    const boom = await send("hi", "steer", async () => {
      throw new Error("Failed to fetch");
    }, t.track, "", () => "msg_first").catch((e) => e);
    expect(boom.replay).toBe("msg_first");

    const bad = answers({ title: "the sandbox refused" }, 502);
    const gone = await send("hi", "steer", bad.post, t.track, "", () => "msg_second").catch((e) => e);
    expect(gone.replay).toBe("msg_second");

    // And the retry actually uses it rather than minting again, which
    // is the half that makes the sandbox's dedupe fire.
    const { post, seen } = answers({ id: "msg_first" });
    await send("hi", "steer", post, t.track, boom.replay, () => "msg_third");
    expect(seen[0].id).toBe("msg_first");
  });
});

describe("a subagent's turns", () => {
  // A subagent runs turns of its own on the root's log, and its end
  // arrives before the root's. The log is the measured one.
  //
  // @decision(DL-111) @decision(DL-059)
  const children = childIds(subagentConversations);
  const midTurn = subagentLog.filter((e) => e.pos < 26);

  it("does not end the root's turn", () => {
    // The child's execution.succeeded (pos 25) is the last execution frame.
    expect(turnInFlight(midTurn)).toBe(false);
    expect(turnInFlight(midTurn, children)).toBe(true);
    expect(turnInFlight(subagentLog, children)).toBe(false);
  });

  it("is not counted as a turn that ended", () => {
    expect(turnsEnded(midTurn)).toBe(1);
    expect(turnsEnded(midTurn, children)).toBe(0);
    expect(turnsEnded(subagentLog, children)).toBe(1);
  });

  it("counts a session no conversation names as a root's", () => {
    expect(turnInFlight(midTurn, childIds([]))).toBe(false);
    expect(turnsEnded(midTurn, childIds([]))).toBe(1);
  });

  it("does not clear the reader's echo, nor does a model it selected become the session's", () => {
    const echo: Echo = { key: "msg_k", id: "msg_k", text: "hi", at: 1000, sending: false };
    const theirs = frame({ message: "msg_k", session: Child });
    expect(unseen([echo], [], [theirs])).toEqual([]);
    expect(unseen([echo], subagentConversations, [theirs])).toHaveLength(1);
    const selected = frame({ name: "session.model.selected", model: "other/child-model", session: Child, pos: 99 });
    expect(currentModel([...subagentLog, selected], "x")).toBe("other/child-model");
    expect(currentModel([...subagentLog, selected], "x", children)).toBe("probe/m");
  });
});

describe("reconciling the echo", () => {
  const echo = (over: Partial<Echo> = {}): Echo => ({
    key: "msg_k",
    id: "msg_k",
    text: "hi",
    at: 1000,
    sending: false,
    ...over,
  });

  // @decision(DL-059) @decision(DL-019)
  it("clears on the stored message", () => {
    expect(unseen([echo()], conv([{ id: "msg_k", role: "user", text: "hi" }]), [])).toEqual([]);
  });

  it("clears on the live frame", () => {
    expect(unseen([echo()], [], [frame({ message: "msg_k" })])).toEqual([]);
  });

  it("clears on the SERVER's id when it differs from the minted one", () => {
    const e = echo({ key: "msg_mine", id: "msg_theirs" });
    expect(unseen([e], [], [frame({ message: "msg_theirs" })])).toEqual([]);
    // And on the minted one too, the fallback when an unreadable answer
    // left no server id.
    expect(unseen([e], [], [frame({ message: "msg_mine" })])).toEqual([]);
  });

  // @decision(DL-059) @decision(DL-017)
  it("keeps an accepted message the recorder has not stored", () => {
    expect(unseen([echo()], conv([{ id: "msg_other", text: "something else" }]), [])).toHaveLength(
      1,
    );
  });

  it("does not match a message with no id at all", () => {
    expect(delivered(conv([{ role: "user", text: "hi" }]), [frame({})])).toEqual(new Set());
  });
});

describe("the echo's own label", () => {
  // Two different facts, and the second is the one worth a line: the
  // sandbox has the message and nothing has recorded it yet.
  it("says which of the two states a row is in", () => {
    const base: Echo = { key: "k", id: "k", text: "t", at: 0, sending: true };
    expect(echoNote(base)).toMatch(/sending/i);
    expect(echoNote({ ...base, sending: false })).toContain("transcript");
  });
});

describe("whether a turn is running", () => {
  /** One execution frame, at the position the store gave it. */
  function exec(pos: number, name: string): Activity {
    return { name, source: "opencode", at: "2026-09-16T10:00:00Z", pos };
  }

  // @decision(DL-059) @decision(DL-053)
  it("says no on an empty log", () => {
    expect(turnInFlight([])).toBe(false);
  });

  it("says yes once a turn has started", () => {
    expect(turnInFlight([exec(1, TurnStarted)])).toBe(true);
  });

  // All three enders, not only the successful one: clearing on
  // `.succeeded` alone would leave Stop on screen after an interrupt.
  //
  // @decision(DL-059) @decision(DL-054)
  it("says no again however the turn ended", () => {
    for (const ender of TurnEnders) {
      expect(turnInFlight([exec(1, TurnStarted), exec(2, ender)]), ender).toBe(false);
    }
  });

  it("says yes again on the next turn", () => {
    const log = [exec(1, TurnStarted), exec(2, "session.execution.succeeded"), exec(3, TurnStarted)];
    expect(turnInFlight(log)).toBe(true);
  });

  // It is the LAST frame by POSITION that decides, not the last one in
  // the array. They agree today — the stream replays in order and
  // resumes from a cursor — but deciding on arrival order makes the
  // control a property of the transport.
  //
  // @decision(DL-059)
  it("decides on the log's own order rather than on arrival", () => {
    const late = [exec(9, TurnStarted), exec(2, "session.execution.succeeded")];
    expect(turnInFlight(late)).toBe(true);
  });

  // Everything else on the page's busiest list is skipped. A session
  // producing hundreds of step and text frames must not have any of
  // them answer this question.
  it("ignores every frame that is not an execution frame", () => {
    const noise = [
      exec(1, TurnStarted),
      exec(2, "session.step.started"),
      exec(3, "session.text.ended"),
      exec(4, "session.inbox.enqueued"),
    ];
    expect(turnInFlight(noise)).toBe(true);
  });
});

describe("how many turns have ended", () => {
  function exec(pos: number, name: string): Activity {
    return { name, source: "opencode", at: "2026-09-16T10:00:00Z", pos };
  }

  // A turn that ends and one that starts in the same batch leave
  // `turnInFlight` true throughout; the count still rises.
  //
  // @decision(DL-059)
  it("counts every ending once, by position, whatever follows it", () => {
    const log = [
      exec(1, TurnStarted),
      exec(2, "session.execution.interrupted"),
      exec(3, TurnStarted),
      exec(2, "session.execution.interrupted"),
      exec(4, "session.step.started"),
    ];
    expect(turnInFlight(log)).toBe(true);
    expect(turnsEnded(log)).toBe(1);
    expect(turnsEnded([...log, exec(5, "session.execution.succeeded")])).toBe(2);
  });
});

describe("whether a turn may be stopped", () => {
  // Mirrors TestTheTwoRefusalsCoverTheSameStates on the Go side. An
  // observation rather than a law: `blocked` is where the two agree
  // for opposite reasons.
  //
  // @decision(DL-059) @decision(DL-053) @decision(DL-052)
  it("refuses the same states a message is refused in", () => {
    for (const state of [undefined, ...KNOWN_JOB_STATES]) {
      expect(canStop(state), state).toBe(sendability(state).can);
    }
  });

  // @decision(DL-059)
  it("lets an unknown state stop", () => {
    expect(canStop("something-new")).toBe(true);
  });
});

describe("what a stop says afterwards", () => {
  it("says the turn was interrupted when one was", () => {
    expect(stopOutcome(true).toLowerCase()).toContain("interrupted");
  });

  // `interrupted: false` does not mean the session was idle; it can
  // also mean a waiting message was delivered and started a turn.
  //
  // @decision(DL-059) @decision(DL-053)
  it("never claims the session was idle", () => {
    const said = stopOutcome(false).toLowerCase();
    expect(said).toContain("either");
    expect(said).toContain("started one");
    for (const claim of ["was idle", "nothing happened"]) {
      expect(said, claim).not.toContain(claim);
    }
    expect(stopOutcome(true)).toContain("Anything you sent is read now.");
  });
});

describe("stopping", () => {
  /** What the API client answers, in the shape openapi-fetch returns. */
  function answers(status: number, body: unknown) {
    return async () => ({
      response: { ok: status >= 200 && status < 300, status },
      ...(status >= 400 ? { error: body } : { data: body as { interrupted?: boolean } }),
    });
  }

  it("reports what the sandbox said", async () => {
    await expect(stop(answers(200, { interrupted: true }))).resolves.toBe(true);
    await expect(stop(answers(200, { interrupted: false }))).resolves.toBe(false);
  });

  // @decision(DL-059) @decision(DL-053)
  it("reads a body it does not recognise as false", async () => {
    await expect(stop(answers(200, {}))).resolves.toBe(false);
  });

  // @decision(DL-059) @decision(DL-053)
  it("throws the server's refusal rather than swallowing it", async () => {
    await expect(
      stop(answers(409, { title: "this job's turn cannot be interrupted" })),
    ).rejects.toThrow(ApiError);
    await expect(
      stop(answers(429, { title: "too many interrupts for this session" })),
    ).rejects.toThrow("too many interrupts");
  });
});

describe("resuming", () => {
  // @decision(DL-059) @decision(DL-054)
  it("is offered exactly where the endpoint accepts it", () => {
    for (const state of [undefined, ...KNOWN_JOB_STATES]) {
      expect(resumable(state), state).toBe(state === "steered" || state === "blocked");
    }
  });

  // Answering in the transcript resumes a blocked job by itself, so the
  // note sends the reader there rather than to `spov attach`.
  //
  // @decision(DL-059) @decision(DL-054)
  it("says which half the button is", () => {
    const blocked = resumeNote("blocked").toLowerCase();
    expect(blocked).toContain("answer it in the transcript");
    expect(blocked).toContain("carries on by itself");
    expect(blocked).toContain("without answering");
    expect(blocked).not.toContain("spov attach");
    expect(resumeNote("steered")).not.toBe(resumeNote("blocked"));
    expect(resumeNote("steered").toLowerCase()).toContain("goal");
  });
});

// @decision(DL-019)
describe("cancelling a queued message", () => {
  it("asks for the message it was given and reports nothing more", async () => {
    const asked: string[] = [];
    await cancel("msg_q", async (id) => {
      asked.push(id);
      return { response: { ok: true, status: 202 } };
    });
    expect(asked).toEqual(["msg_q"]);
  });

  it("throws the server's refusal", async () => {
    await expect(
      cancel("msg_q", async () => ({ response: { ok: false, status: 409 }, error: { title: "Conflict", status: 409 } })),
    ).rejects.toBeInstanceOf(ApiError);
  });
});

// @decision(DL-048) @decision(DL-019)
describe("whether a queued message can still be read", () => {
  it("is closed once the stream, the session or the job's run is over", () => {
    expect(inboxClosed("running", false, "running")).toBe(false);
    expect(inboxClosed("running", false, "steered")).toBe(false);
    expect(inboxClosed("running", false, undefined)).toBe(false);
    expect(inboxClosed("running", true, "running")).toBe(true);
    expect(inboxClosed("stopped", false, undefined)).toBe(true);
    for (const state of ["delivering", "succeeded", "failed", "cancelled"] as const) {
      expect(inboxClosed("running", false, state), state).toBe(true);
    }
  });
});

// @decision(DL-019) @decision(DL-059)
describe("the line under a waiting message", () => {
  it("says when it is read and what follows", () => {
    expect(queuedNote("queue")).toContain("when its current turn ends");
    expect(queuedNote("steer")).toContain("once its current step finishes");
    expect(queuedNote("queue", "handover")).toContain("pauses for you");
    expect(queuedNote(undefined, "continue")).not.toContain("pauses");
  });
});

// @decision(DL-059)
describe("the status bar above the box", () => {
  it("says what the agent is doing and whose the job is", () => {
    expect(composerStatus("running", true)?.word).toBe("Working on a step");
    expect(composerStatus("steered", true)?.word).toBe("Working on a step · paused for you");
    expect(composerStatus("steered", false)).toEqual({ mark: "wait", word: "Paused for you" });
    expect(composerStatus("blocked", false)?.word).toBe("Waiting on a question");
    expect(composerStatus(undefined, true)?.word).toBe("Working on a step");
  });

  // @decision(DL-020)
  it("says first that an ask waits on the reader", () => {
    expect(composerStatus("blocked", false, true)).toEqual({ mark: "wait", word: "Waiting on your answer" });
    expect(composerStatus(undefined, true, true)?.word).toBe("Waiting on your answer");
  });

  it("is not drawn when there is nothing to say", () => {
    expect(composerStatus("running", false)).toBeNull();
    expect(composerStatus(undefined, false)).toBeNull();
  });

  it("explains only the controls that are drawn", () => {
    expect(controlsNote("steered", false, false)).toEqual([]);
    expect(controlsNote("running", true, false).map((l) => l.control)).toEqual(["Interrupt"]);
    // Each line names the button it is about.
    const both = controlsNote("steered", true, true);
    expect(both.map((l) => l.control)).toEqual(["Interrupt", "Resume job"]);
    expect(both[1].does).toBe(resumeNote("steered"));
  });
});
