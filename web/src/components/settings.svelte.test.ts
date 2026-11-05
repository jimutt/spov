import { describe, expect, it, vi } from "vitest";
import { flushSync, mount, unmount } from "svelte";

/**
 * The Settings page, mounted, for which buttons are disabled while a
 * request is in flight — only the rendered markup knows that.
 *
 * @decision(DL-062) @decision(DL-014)
 */

const profile = {
  user_id: "u",
  org_id: "o",
  admin: false,
  default_model: "anthropic/claude-sonnet-5",
  git_name: "Ada",
  git_email: "ada@example.com",
  quota: {
    max_concurrent_sessions: 3,
    max_concurrent_jobs: 2,
    max_timeout: "8h0m0s",
    max_cpu: 4,
    max_memory_mib: 16384,
  },
};

/**
 * Installed by vi.hoisted, which is the load-bearing part: openapi-fetch
 * binds `globalThis.fetch` when the client is CREATED, and `client.ts`
 * creates it at import. A stub installed in the test body is captured by
 * nothing and every request goes to the real network.
 */
const stub = vi.hoisted(() => {
  const json = (body: unknown) =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  let release!: () => void;
  const held = new Promise<Response>((resolve) => {
    release = () =>
      resolve(
        json({
          user_id: "u",
          org_id: "o",
          admin: false,
          default_model: "anthropic/claude-opus-5",
          git_name: "Grace",
          git_email: "ada@example.com",
          quota: {
            max_concurrent_sessions: 3,
            max_concurrent_jobs: 2,
            max_timeout: "8h0m0s",
            max_cpu: 4,
            max_memory_mib: 16384,
          },
        }),
      );
  });
  const fetched = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input instanceof Request ? input.url : input);
    const method = init?.method ?? (input instanceof Request ? input.method : "GET");
    // Never answers, so "in flight" is a state the test can hold.
    if (method === "PATCH") return held;
    if (url.includes("/api/v1/models")) {
      return json({
        providers: [
          {
            provider: "anthropic",
            name: "Anthropic",
            key_env: "ANTHROPIC_API_KEY",
            have_key: true,
            models: ["anthropic/claude-sonnet-5", "anthropic/claude-opus-5"],
          },
        ],
      });
    }
    if (url.includes("/api/v1/me/credentials")) {
      return json({ enabled: true, credentials: [] });
    }
    if (url.includes("/api/v1/me")) {
      return json({
        user_id: "u",
        org_id: "o",
        admin: false,
        default_model: "anthropic/claude-sonnet-5",
        git_name: "Ada",
        git_email: "ada@example.com",
        quota: {
          max_concurrent_sessions: 3,
          max_concurrent_jobs: 2,
          max_timeout: "8h0m0s",
          max_cpu: 4,
          max_memory_mib: 16384,
        },
      });
    }
    return new Response(null, { status: 404 });
  });
  globalThis.fetch = fetched as unknown as typeof globalThis.fetch;
  return { fetched, release: () => release() };
});

// Imported AFTER the hoisted stub is installed, which is what the
// hoisting is for.
import Settings from "./Settings.svelte";

/** Lets the resources' promises land before the next assertion. */
async function settle() {
  for (let i = 0; i < 30; i++) await Promise.resolve();
  flushSync();
}

/**
 * The two PROFILE saves, in page order. Scoped by form rather than by
 * `button[type=submit]`, which would also pick up the credentials
 * section's Store — a different endpoint and a different row, so
 * deliberately not part of this guard.
 */
const saves = (root: HTMLElement) =>
  ["#model-form", "#identity-form"].map(
    (id) => root.querySelector(`${id} button[type=submit]`) as HTMLButtonElement,
  );
const patches = () =>
  stub.fetched.mock.calls.filter(
    ([input, init]) =>
      (init?.method ?? (input instanceof Request ? input.method : "GET")) === "PATCH",
  ).length;

describe("the Settings page's save buttons", () => {
  it("blocks BOTH while either save is in flight", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(Settings, { target });
    await settle();

    const name = target.querySelector("#git-name") as HTMLInputElement;
    const model = target.querySelector("#settings-model") as HTMLButtonElement;
    expect(name.value).toBe(profile.git_name);
    expect(model.textContent).toContain(profile.default_model);

    // Both forms dirty, so both buttons are enabled on their own
    // account and what follows is about the shared guard rather than
    // about nothing having changed.
    name.value = "Grace";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    // The model picker: open it, then choose an option.
    model.click();
    flushSync();
    const option = Array.from(target.querySelectorAll('[data-testid="model-picker-option"]')).find(
      (o) => o.textContent?.trim() === "claude-opus-5",
    ) as HTMLButtonElement;
    option.click();
    flushSync();
    expect(saves(target).map((b) => b.disabled)).toEqual([false, false]);

    // One save starts and does not answer.
    saves(target)[1].click();
    await settle();
    expect(patches()).toBe(1);

    expect(saves(target).map((b) => b.disabled)).toEqual([true, true]);

    // And the guard is not only the attribute. A disabled button
    // dispatches nothing, so clicking it would prove nothing either —
    // the form is submitted DIRECTLY, which is what a stray Enter, an
    // extension or a `requestSubmit` does, and the handler's own check
    // is the half that has to stop it.
    const form = target.querySelector("#model-form") as HTMLFormElement;
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await settle();
    expect(patches()).toBe(1);

    stub.release();
    await settle();
    unmount(app);
    target.remove();
  });
});
