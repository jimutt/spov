import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, tick, unmount } from "svelte";

/**
 * The resume composer, mounted: the markup and the guards, since the
 * wording is resume.ts's and tested there.
 *
 * @decision(DL-090) @decision(DL-061)
 */
const stub = vi.hoisted(() => {
  const bodies: Record<string, unknown>[] = [];
  let answer: () => Response = () =>
    new Response(JSON.stringify({ id: "s1", state: "starting" }), {
      status: 202,
      headers: { "content-type": "application/json" },
    });
  const sent = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const raw = await req.text();
    bodies.push((raw ? JSON.parse(raw) : {}) as Record<string, unknown>);
    return answer();
  });
  globalThis.fetch = sent as unknown as typeof globalThis.fetch;
  return {
    sent,
    bodies: () => bodies,
    answerWith: (f: () => Response) => (answer = f),
    reset: () => {
      bodies.splice(0, bodies.length);
      sent.mockClear();
      answer = () =>
        new Response(JSON.stringify({ id: "s1", state: "starting" }), {
          status: 202,
          headers: { "content-type": "application/json" },
        });
    },
  };
});

import ResumeComposer from "./ResumeComposer.svelte";

function render(onResumed = vi.fn()) {
  const target = document.createElement("div");
  document.body.append(target);
  const c = mount(ResumeComposer, {
    target,
    props: { ref: "s1", resume: { mode: "transcript" }, onResumed },
  });
  return {
    target,
    onResumed,
    type(text: string) {
      const box = target.querySelector("textarea")!;
      box.value = text;
      box.dispatchEvent(new Event("input"));
      flushSync();
    },
    submit() {
      target.querySelector("form")!.dispatchEvent(new Event("submit", { cancelable: true }));
      flushSync();
    },
    done: () => {
      unmount(c);
      target.remove();
    },
  };
}

const settle = async () => {
  for (let i = 0; i < 5; i++) {
    await tick();
    await new Promise((r) => setTimeout(r, 0));
  }
};

describe("ResumeComposer", () => {
  beforeEach(() => stub.reset());

  it("asks first, worded for a transcript resume, and sends the message with the resume", async () => {
    const r = render();
    r.type("carry on");
    r.submit();
    expect(stub.sent).not.toHaveBeenCalled();
    expect(r.target.textContent).toContain("cloned fresh");
    (r.target.querySelector('[data-testid="resume-composer-yes"]') as HTMLButtonElement).click();
    await settle();
    expect(stub.sent).toHaveBeenCalledTimes(1);
    expect((stub.bodies()[0].message as { text: string }).text).toBe("carry on");
    expect((stub.bodies()[0].message as { id: string }).id).toMatch(/^msg_/);
    expect(r.onResumed).toHaveBeenCalled();
    r.done();
  });

  it("never submits on Enter", () => {
    const r = render();
    r.type("carry on");
    r.target.querySelector("textarea")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    flushSync();
    expect(stub.sent).not.toHaveBeenCalled();
    expect(r.target.querySelector('[data-testid="resume-composer-ask"]')).toBeNull();
    r.done();
  });

  it("keeps Resume and send off after an answer it cannot read", async () => {
    stub.answerWith(() => new Response("{}", { status: 202, headers: { "content-type": "application/json" } }));
    const r = render();
    r.type("carry on");
    r.submit();
    (r.target.querySelector('[data-testid="resume-composer-yes"]') as HTMLButtonElement).click();
    await settle();
    expect(r.target.textContent).toContain("Reload the page");
    const yes = r.target.querySelector('[data-testid="resume-composer-yes"]') as HTMLButtonElement;
    expect(yes.disabled).toBe(true);
    r.submit();
    expect(stub.sent).toHaveBeenCalledTimes(1);
    r.done();
  });
});
