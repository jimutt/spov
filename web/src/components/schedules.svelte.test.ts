import { afterEach, describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The schedule form and a schedule's page, mounted, for what only the
 * wiring decides: a refused save stays on the form and says why, a save
 * leaves for the schedule, and Run now shows its job or its refusal.
 *
 * @decision(DL-195)
 */

const stub = vi.hoisted(() => {
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  const view = {
    id: "01SCHED",
    name: "weekly-deps",
    owner_id: "u",
    cron: "0 9 * * 1",
    zone: "UTC",
    state: "active",
    next_run_at: "2026-11-30T09:00:00Z",
    next_runs: ["2026-11-30T09:00:00Z", "2026-12-07T09:00:00Z", "2026-12-14T09:00:00Z"],
    failures: 0,
    job: { goal: "report on stale deps", kind: "report", repos: [{ platform: "github", owner: "o", name: "r" }] },
    created_at: "2026-11-27T09:00:00Z",
    updated_at: "2026-11-27T09:00:00Z",
    history: [],
  };
  const state = { createStatus: 201, runStatus: 202 };
  const calls: { method: string; url: string; body?: unknown }[] = [];
  const fetched = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(String(input), init);
    const url = req.url;
    const method = req.method;
    const text = method === "GET" ? "" : await req.text();
    calls.push({ method, url, body: text ? JSON.parse(text) : undefined });
    if (url.includes("/api/v1/repos")) return json({ repos: [{ platform: "github", owner: "o", name: "r" }], platforms: [] });
    if (url.includes("/api/v1/models")) return json({ providers: [] });
    if (url.includes("/api/v1/me/templates")) return json({ templates: [] });
    if (url.includes("/api/v1/me")) return json({ user_id: "u", org_id: "o", admin: false });
    if (url.endsWith("/api/v1/schedules") && method === "POST") {
      return state.createStatus === 201
        ? json(view, 201)
        : json({ title: "Unprocessable Entity", status: 422, detail: "the cadence fires 30m0s apart at its closest" }, 422);
    }
    if (url.includes("/run") && method === "POST") {
      return state.runStatus === 202
        ? json({ job_id: "01JOB" }, 202)
        : json({ title: "Conflict", status: 409, detail: "not run: the previous run is still running" }, 409);
    }
    if (url.includes("/api/v1/schedules/")) return json(view);
    return new Response(null, { status: 404 });
  });
  globalThis.fetch = fetched as unknown as typeof globalThis.fetch;
  return { state, calls };
});

import { nav } from "../lib/api/client";
import ScheduleForm from "./ScheduleForm.svelte";
import ScheduleView from "./ScheduleView.svelte";

async function settle() {
  for (let i = 0; i < 40; i++) await Promise.resolve();
  flushSync();
}

const mounted: { app: Record<string, unknown>; target: HTMLElement }[] = [];
afterEach(() => {
  for (const m of mounted.splice(0)) {
    unmount(m.app);
    m.target.remove();
  }
  stub.calls.length = 0;
  stub.state.createStatus = 201;
  stub.state.runStatus = 202;
  vi.restoreAllMocks();
});

function open(component: typeof ScheduleForm | typeof ScheduleView, props: Record<string, unknown> = {}) {
  const target = document.createElement("div");
  document.body.append(target);
  const app = mount(component as typeof ScheduleForm, { target, props });
  mounted.push({ app, target });
  return target;
}

async function fillNewSchedule(target: HTMLElement) {
  const name = target.querySelector("#schedule-name") as HTMLInputElement;
  name.value = "weekly-deps";
  name.dispatchEvent(new Event("input", { bubbles: true }));
  const goal = target.querySelector("#job-goal") as HTMLTextAreaElement;
  goal.value = "report on stale deps";
  goal.dispatchEvent(new Event("input", { bubbles: true }));
  const repo = Array.from(target.querySelectorAll('input[type="checkbox"]'))[0] as HTMLInputElement | undefined;
  repo?.click();
  await settle();
}

describe("the schedule form", () => {
  it("stays and says why when the save is refused", async () => {
    stub.state.createStatus = 422;
    const replace = vi.spyOn(nav, "replace").mockImplementation(() => {});
    const target = open(ScheduleForm);
    await settle();
    await fillNewSchedule(target);
    const form = target.querySelector("form") as HTMLFormElement;
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await settle();
    expect(stub.calls.some((c) => c.method === "POST" && c.url.endsWith("/api/v1/schedules"))).toBe(true);
    expect(replace).not.toHaveBeenCalled();
    expect(target.textContent).toContain("30m0s apart");
    const save = target.querySelector('button[type="submit"]') as HTMLButtonElement;
    expect(save.disabled).toBe(false);
  });

  it("leaves for the schedule's page by its id once saved", async () => {
    const replace = vi.spyOn(nav, "replace").mockImplementation(() => {});
    const target = open(ScheduleForm);
    await settle();
    await fillNewSchedule(target);
    (target.querySelector("form") as HTMLFormElement).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await settle();
    const post = stub.calls.find((c) => c.method === "POST");
    expect(post?.body).toMatchObject({ name: "weekly-deps", cron: "0 9 * * *" });
    expect(replace).toHaveBeenCalledWith("/schedules/view?ref=01SCHED");
  });
});

describe("a schedule's page", () => {
  function confirmRun(target: HTMLElement) {
    (target.querySelector('[data-testid="schedule-run"]') as HTMLButtonElement).click();
    flushSync();
    (target.querySelector('[data-testid="schedule-run-yes"]') as HTMLButtonElement).click();
  }

  it("shows the job a run started", async () => {
    history.replaceState(null, "", "/schedules/view?ref=01SCHED");
    const target = open(ScheduleView);
    await settle();
    expect(target.querySelector('[data-testid="schedule-name"]')?.textContent).toBe("weekly-deps");
    expect(target.querySelector('[data-testid="schedule-next"]')?.textContent).toContain("09:00");
    confirmRun(target);
    await settle();
    const link = target.querySelector('[data-testid="schedule-started"] a') as HTMLAnchorElement | null;
    expect(link?.getAttribute("href")).toBe("/session/?id=job-01JOB");
  });

  it("says why a run did not start", async () => {
    stub.state.runStatus = 409;
    history.replaceState(null, "", "/schedules/view?ref=01SCHED");
    const target = open(ScheduleView);
    await settle();
    confirmRun(target);
    await settle();
    expect(target.querySelector('[data-testid="schedule-started"]')).toBeNull();
    expect(target.textContent).toContain("still running");
  });
});
