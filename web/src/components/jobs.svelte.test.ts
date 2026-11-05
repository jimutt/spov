import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The jobs page, mounted, for what only the markup and its reactivity
 * can get wrong: which listing a filter asks for, the URL it leaves
 * behind, and never drawing one filter's rows under another's name.
 *
 * @decision(DL-060)
 */

const UNFINISHED_QUERY = "queued,provisioning,running,delivering,blocked,steered";

const stub = vi.hoisted(() => {
  const asked: string[] = [];
  const befores: string[] = [];
  // While set, every listing but the count never answers.
  let hold = false;
  const row = (
    id: string,
    state: string,
    extra: Record<string, unknown> = {},
  ) => ({
    id,
    session_id: `ses_${id}`,
    owner_id: "u",
    kind: "pr",
    goal: `goal of ${id}\nsecond line`,
    state,
    attempt: 1,
    created_at: "2026-09-26T10:00:00Z",
    updated_at: "2026-09-26T10:00:00Z",
    ...extra,
  });
  const rows = [
    row("a", "running"),
    row("b", "blocked", { error: "waiting on a permission" }),
    row("c", "succeeded", {
      deliverables: [
        { kind: "pr", status: "verified", url: "https://forge.example/acme/app/pulls/7", number: 7 },
      ],
    }),
  ];
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const req = input instanceof Request ? input : new Request(String(input));
    const url = new URL(req.url);
    if (url.pathname === "/api/v1/me")
      return json({ id: "u", quota: { max_concurrent_sessions: 3, max_concurrent_jobs: 2 } });
    if (url.pathname !== "/api/v1/jobs")
      return new Response(null, { status: 404 });
    const state = url.searchParams.get("state") ?? "";
    asked.push(state);
    const wanted = state ? state.split(",") : undefined;
    // The unfiltered listing has one older page behind its first.
    // @decision(DL-060)
    const before = url.searchParams.get("before");
    if (before) {
      befores.push(before);
      return json({ jobs: [row("old", "succeeded")], total: 4 });
    }
    const matching = rows.filter((r) => !wanted || wanted.includes(r.state));
    const answer = json({
      jobs: matching,
      total: state ? matching.length : 4,
      ...(state ? {} : { next: "c" }),
    });
    if (hold && state !== UNFINISHED_QUERY) {
      return new Promise<Response>(() => {});
    }
    return answer;
  }) as unknown as typeof globalThis.fetch;

  return {
    asked: () => asked,
    befores: () => befores,
    reset: () => {
      asked.splice(0, asked.length);
      befores.splice(0, befores.length);
      hold = false;
    },
    hold: () => (hold = true),
  };
});

import Jobs from "./Jobs.svelte";

beforeEach(() => stub.reset());

async function settle() {
  for (let i = 0; i < 60; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  for (let i = 0; i < 20; i++) await Promise.resolve();
  flushSync();
}

async function open(url = "/jobs/") {
  history.replaceState({}, "", url);
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(Jobs, { target });
  await settle();
  const q = (id: string) =>
    target.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;
  return {
    q,
    rows: () => Array.from(target.querySelectorAll('[data-testid="jobs-row"]')),
    text: () => target.textContent ?? "",
    done: () => {
      unmount(app);
      target.remove();
    },
  };
}

describe("the jobs page", () => {
  // @decision(DL-060)
  it("shows every job by default, with the count of those waiting on you", async () => {
    const page = await open();
    expect(stub.asked()).toContain("");
    expect(page.rows()).toHaveLength(3);
    expect(page.q("jobs-needs-you-count")?.textContent).toBe("1");
    // Running and blocked both count against the limit. @decision(DL-060)
    expect(stub.asked()).toContain(UNFINISHED_QUERY);
    expect(page.q("limit-jobs")?.textContent?.replace(/\s+/g, " ").trim()).toBe("2 of 2 jobs in progress");
    expect(page.q("limit-jobs")?.getAttribute("title")).toContain("At your limit");
    // The first line of the goal, the pull request, what it waits on.
    expect(page.text()).toContain("goal of a");
    expect(page.text()).not.toContain("second line");
    expect(page.text()).toContain("https://forge.example/acme/app/pulls/7");
    expect(page.text()).toContain("waiting on a permission");
    const link = page
      .rows()[0]
      .querySelector('a[href^="/session/"]') as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/session/?id=ses_a");
    page.done();
  });

  // @decision(DL-060)
  it("asks for a filter's states and keeps the filter in the URL", async () => {
    const page = await open();
    page.q("jobs-show-finished")!.click();
    await settle();
    expect(stub.asked()).toContain("succeeded,failed,cancelled");
    expect(location.search).toBe("?show=finished");
    expect(page.rows()).toHaveLength(1);

    page.q("jobs-show-all")!.click();
    await settle();
    expect(location.search).toBe("");
    page.done();
  });

  // @decision(DL-060)
  it("opens on the filter the URL names", async () => {
    const page = await open("/jobs/?show=needs-you");
    expect(stub.asked()).toContain("blocked,steered");
    expect(page.q("jobs-show-needs-you")?.getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(page.rows()).toHaveLength(1);
    page.done();
  });

  // @decision(DL-060)
  it("never draws one filter's rows under another's name", async () => {
    const page = await open();
    expect(page.rows()).toHaveLength(3);
    stub.hold();
    page.q("jobs-show-active")!.click();
    await settle();
    // The All rows are gone while Active has not answered.
    expect(page.rows()).toHaveLength(0);
    expect(page.text()).toContain("Loading");
    page.done();
  });

  // @decision(DL-060)
  it("appends the next page on request and drops it on a filter switch", async () => {
    const page = await open();
    const older = () => page.q("jobs-older") as HTMLButtonElement | null;
    expect(older()?.textContent?.trim()).toBe("Show older (1 more)");
    older()!.click();
    await settle();
    expect(stub.befores()).toEqual(["c"]);
    expect(page.rows()).toHaveLength(4);
    expect(page.text()).toContain("goal of old");
    expect(older(), "the last page still offers more").toBeNull();

    page.q("jobs-show-finished")!.click();
    await settle();
    expect(page.text()).not.toContain("goal of old");
    page.done();
  });
});

