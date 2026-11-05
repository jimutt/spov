import { describe, expect, it } from "vitest";
import {
  Other,
  type Ask,
  type FormField,
  answer,
  answerSummary,
  askLine,
  askStates,
  emptyDraft,
  pendingAsks,
  permissionSummary,
  shownFields,
  toAnswer,
  webLink,
} from "./asks";
import { blocksOf, buildTimeline, condense, isAudit } from "./timeline";
import type { Activity } from "./timeline";
import { ApiError } from "./api/client";

let pos = 0;
function frame(name: string, ask: Ask, source = "ephemeral"): Activity {
  pos++;
  return { name, source, ask, at: new Date(1_000 * pos).toISOString(), pos } as Activity;
}

const permission: Ask = { id: "per_1", kind: "permission", action: "glob", resources: ["*.txt"], call: "call_1" };
const colour: FormField = {
  key: "q0",
  type: "string",
  title: "Colour",
  description: "Which colour?",
  options: [
    { value: "Red", label: "Red" },
    { value: "Blue", label: "Blue" },
  ],
  custom: true,
};
const notes: FormField = { key: "q1", type: "multiselect", title: "Notes", options: [{ value: "No", label: "No" }] };
const form: Ask = { id: "frm_1", kind: "form", title: "Questions", fields: [colour, notes] };

// @decision(DL-020)
describe("an ask's status", () => {
  it("is pending from its asking until a reply settles it", () => {
    const asked = [frame("permission.asked", permission), frame("form.created", form)];
    let states = askStates(asked, false);
    expect(states.get("per_1")?.status).toBe("pending");
    expect(pendingAsks(states)).toBe(2);

    states = askStates(
      [
        ...asked,
        frame("permission.replied", { id: "per_1", kind: "permission", reply: "reject" }),
        frame("form.replied", { id: "frm_1", kind: "form", answer: { q0: "Blue", q1: ["No"] } }),
      ],
      false,
    );
    expect(states.get("per_1")?.status).toBe("rejected");
    expect(states.get("frm_1")).toEqual({ status: "answered", answer: { q0: "Blue", q1: ["No"] } });
    expect(pendingAsks(states)).toBe(0);
  });

  it("reads each reply and a dismissal", () => {
    const once = askStates(
      [frame("permission.asked", permission), frame("permission.replied", { ...permission, reply: "once" })],
      false,
    );
    expect(once.get("per_1")?.status).toBe("allowed");
    const always = askStates(
      [frame("permission.asked", permission), frame("permission.replied", { ...permission, reply: "always" })],
      false,
    );
    expect(always.get("per_1")?.status).toBe("allowed-always");
    const gone = askStates([frame("form.created", form), frame("form.cancelled", { id: "frm_1", kind: "form" })], false);
    expect(gone.get("frm_1")?.status).toBe("dismissed");
  });

  it("is not answered once the session has ended, and a reply never revives it", () => {
    const states = askStates([frame("permission.asked", permission)], true);
    expect(states.get("per_1")?.status).toBe("unanswered");
    expect(pendingAsks(states)).toBe(0);
  });

  // An interrupt drops a pending permission with no reply (spike S-F1).
  it("is not answered once a turn of its conversation ends after it", () => {
    const end = (session: string) =>
      ({ name: "session.execution.interrupted", source: "opencode", session, at: "", pos: ++pos }) as Activity;
    const ask = { ...frame("permission.asked", permission), session: "ses_a" };
    expect(askStates([ask, end("ses_b")], false).get("per_1")?.status).toBe("pending");
    expect(askStates([ask, end("ses_a")], false).get("per_1")?.status).toBe("unanswered");
    expect(askStates([end("ses_a"), { ...ask, pos: ++pos }], false).get("per_1")?.status).toBe("pending");
  });

  it("ignores a settling frame with no ask, and frames not from the sandbox", () => {
    const states = askStates(
      [
        frame("permission.replied", { id: "per_9", kind: "permission", reply: "once" }),
        frame("permission.asked", { ...permission, id: "per_2" }, "spov"),
      ],
      false,
    );
    expect(states.size).toBe(0);
  });
});

// @decision(DL-020)
describe("an ask in the transcript", () => {
  // The call's later events join its first line, so the card sits
  // under the call it holds up.
  it("is one card where it was asked, never folded into a run", () => {
    const activity: Activity[] = [
      { name: "session.tool.input.started", source: "opencode", call: "call_1", tool: "glob", at: "", pos: 1 } as Activity,
      { ...frame("permission.asked", permission), pos: 2 },
      { name: "session.tool.success", source: "opencode", call: "call_1", at: "", pos: 3 } as Activity,
      { ...frame("permission.replied", { ...permission, reply: "once" }), pos: 4 },
    ];
    const blocks = blocksOf(condense(buildTimeline([], activity)));
    expect(blocks.map((b) => b.kind)).toEqual(["events", "ask"]);
    const ask = blocks[1];
    expect(ask.kind === "ask" && ask.entry.ask.id).toBe("per_1");
  });

  it("hides the answer's audit markers", () => {
    for (const name of [
      "spov.session.permission.replied",
      "spov.session.form.answered",
      "spov.session.form.dismissed",
      "spov.job.answered",
    ]) {
      expect(isAudit({ name, source: "spov", at: "", pos: 1 } as Activity), name).toBe(true);
    }
  });

  it("collapses to one line once settled", () => {
    expect(askLine(permission, { status: "allowed" })).toBe("Allowed once");
    expect(askLine(permission, { status: "rejected" })).toBe("Rejected");
    expect(askLine(form, { status: "answered", answer: { q1: ["No"], q0: "Blue" } })).toBe("Answered: Blue, No");
    expect(askLine(form, { status: "dismissed" })).toBe("Dismissed");
    expect(askLine(form, { status: "unanswered" })).toBe("Not answered");
    expect(permissionSummary(permission)).toBe("glob *.txt");
    expect(answerSummary([], { a: true, b: 3 })).toBe("Yes, 3");
  });
});

// @decision(DL-020)
describe("a question form", () => {
  it("builds the answer keyed by field, with Other as the free text", () => {
    const draft = emptyDraft(form.fields!);
    draft.q0.picks = [Other];
    draft.q0.other = " Green ";
    draft.q1.picks = ["No"];
    expect(toAnswer(form.fields!, draft)).toEqual({ answer: { q0: "Green", q1: ["No"] }, problems: [] });
    draft.q1.picks = ["No", Other];
    draft.q1.other = "maybe";
    expect(toAnswer(form.fields!, draft).answer.q1).toEqual(["No", "maybe"]);
  });

  it("says what stops a send", () => {
    const draft = emptyDraft(form.fields!);
    expect(toAnswer(form.fields!, draft).problems).toEqual(["Answer at least one question."]);
    draft.q0.picks = [Other];
    expect(toAnswer(form.fields!, draft).problems).toEqual(["Say what “Other” is for “Colour”."]);
    const required: FormField[] = [{ ...colour, required: true }, { key: "n", type: "integer", title: "Count" }];
    const d = emptyDraft(required);
    d.n.text = "1.5";
    expect(toAnswer(required, d).problems).toEqual(["“Colour” needs an answer.", "“Count” needs a whole number."]);
  });

  it("starts from each field's default and reads the other types", () => {
    const fields: FormField[] = [
      { key: "a", type: "string", default: "hello" },
      { key: "b", type: "boolean", default: false },
      { key: "c", type: "number", default: 2.5 },
      { key: "d", type: "multiselect", options: [{ value: "x", label: "x" }], default: ["x"] },
      { key: "e", type: "external", url: "https://example.com" },
      { key: "f", type: "date-picker" },
    ];
    expect(toAnswer(fields, emptyDraft(fields)).answer).toEqual({ a: "hello", b: false, c: 2.5, d: ["x"] });
  });

  it("shows a field only while its conditions hold, and never a hidden one", () => {
    const fields: FormField[] = [
      colour,
      { key: "why", type: "string", title: "Why red?", when: [{ key: "q0", op: "eq", value: "Red" }] },
      { key: "secret", type: "string", hidden: true },
    ];
    const draft = emptyDraft(fields);
    expect(shownFields(fields, draft).map((f) => f.key)).toEqual(["q0"]);
    draft.q0.picks = ["Red"];
    draft.why.text = "warm";
    expect(shownFields(fields, draft).map((f) => f.key)).toEqual(["q0", "why"]);
    draft.q0.picks = ["Blue"];
    expect(toAnswer(fields, draft).answer).toEqual({ q0: "Blue" });
  });
});

// A field's key and link are the sandbox's to write.
//
// @decision(DL-020)
describe("what the sandbox writes into a form", () => {
  it("is linked only when it is a web address", () => {
    expect(webLink("https://example.com/a")).toBe("https://example.com/a");
    for (const bad of ["javascript:alert(1)", " JAVASCRIPT:alert(1)", "data:text/html,x", "/relative", "", undefined]) {
      expect(webLink(bad), String(bad)).toBeNull();
    }
  });

  it("cannot reach a prototype, and a repeated key is answered once", () => {
    const fields: FormField[] = [
      { key: "__proto__", type: "string", title: "p" },
      { key: "q", type: "string" },
      { key: "q", type: "string" },
    ];
    const draft = emptyDraft(fields);
    draft["__proto__"].text = "x";
    draft.q.text = "y";
    const { answer } = toAnswer(fields, draft);
    expect(Object.keys(answer)).toEqual(["__proto__", "q"]);
    expect(JSON.parse(JSON.stringify(answer))).toEqual(JSON.parse('{"__proto__":"x","q":"y"}'));
    expect(({} as Record<string, unknown>).x).toBeUndefined();
  });
});

// @decision(DL-020)
describe("sending an answer", () => {
  it("throws the server's refusal", async () => {
    await expect(answer(async () => ({ response: { ok: true, status: 202 } }))).resolves.toBeUndefined();
    await expect(
      answer(async () => ({ response: { ok: false, status: 404 }, error: { title: "Not Found", status: 404 } })),
    ).rejects.toBeInstanceOf(ApiError);
  });
});
