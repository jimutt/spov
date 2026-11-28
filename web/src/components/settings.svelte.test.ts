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
  // Whether /me says the caller is an admin.
  const state = { admin: false, defaultModel: "anthropic/claude-sonnet-5" };
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
            variants: { "anthropic/claude-sonnet-5": ["low", "high"], "anthropic/claude-opus-5": ["low"] },
          },
        ],
      });
    }
    if (url.includes("/api/v1/service-accounts")) return json({ accounts: [] });
    if (url.includes("/api/v1/allowlist")) return json({ entries: [] });
    if (url.includes("/api/v1/me/credentials")) {
      return json({ enabled: true, credentials: [] });
    }
    if (url.includes("/api/v1/me")) {
      return json({
        user_id: "u",
        org_id: "o",
        admin: state.admin,
        default_model: state.defaultModel,
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
  return { fetched, release: () => release(), state };
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

/**
 * A non-admin's page draws no People section and never asks for the
 * allowlist.
 *
 * @decision(DL-129)
 */
describe("the People section on the Settings page", () => {
  it("is absent for a non-admin, who never reads the allowlist", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(Settings, { target });
    await settle();
    const asked = stub.fetched.mock.calls.some(([input]) =>
      String(input instanceof Request ? input.url : input).includes("/api/v1/allowlist"),
    );
    expect(asked).toBe(false);
    expect([...target.querySelectorAll("h2")].map((h) => h.textContent)).not.toContain("People");
    unmount(app);
    target.remove();
  });
});

/**
 * A non-admin's page draws no Service accounts section and never asks
 * for the accounts.
 *
 * @decision(DL-142)
 */
describe("the Service accounts section on the Settings page", () => {
  it("is absent for a non-admin, who never reads the accounts", async () => {
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(Settings, { target });
    await settle();
    const asked = stub.fetched.mock.calls.some(([input]) =>
      String(input instanceof Request ? input.url : input).includes("/api/v1/service-accounts"),
    );
    expect(asked).toBe(false);
    expect([...target.querySelectorAll("h2")].map((h) => h.textContent)).not.toContain("Service accounts");
    unmount(app);
    target.remove();
  });

  it("is drawn for an admin", async () => {
    stub.state.admin = true;
    try {
      const target = document.createElement("div");
      document.body.append(target);
      const app = mount(Settings, { target });
      await settle();
      await settle();
      expect([...target.querySelectorAll("h2")].map((h) => h.textContent)).toContain("Service accounts");
      unmount(app);
      target.remove();
    } finally {
      stub.state.admin = false;
    }
  });
});

/**
 * The default model's reasoning mode: seeded from the stored default's
 * #mode, kept across a choice of a model that offers it, back to
 * Default for one that does not, and a mode no longer offered drawn
 * disabled with a note.
 *
 * @decision(DL-189)
 */
describe("the default model's reasoning choice on the Settings page", () => {
  async function open(defaultModel: string) {
    stub.state.defaultModel = defaultModel;
    const target = document.createElement("div");
    document.body.append(target);
    const app = mount(Settings, { target });
    await settle();
    return {
      target,
      reasoning: () => target.querySelector("#settings-reasoning") as HTMLSelectElement | null,
      pick: (label: string) => {
        (target.querySelector("#settings-model") as HTMLButtonElement).click();
        flushSync();
        (Array.from(target.querySelectorAll('[data-testid="model-picker-option"]')).find(
          (o) => o.textContent?.trim() === label,
        ) as HTMLButtonElement).click();
        flushSync();
      },
      done: () => {
        unmount(app);
        target.remove();
        stub.state.defaultModel = "anthropic/claude-sonnet-5";
      },
    };
  }

  it("seeds the mode from the stored default and follows the model", async () => {
    const page = await open("anthropic/claude-sonnet-5#high");
    try {
      await seededAndFollowed(page);
    } finally {
      page.done();
    }
  });

  async function seededAndFollowed(page: Awaited<ReturnType<typeof open>>) {
    const picker = page.target.querySelector("#settings-model") as HTMLButtonElement;
    expect(picker.textContent).toContain("anthropic/claude-sonnet-5");
    expect(picker.textContent).not.toContain("#high");
    const select = page.reasoning()!;
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "low", "high"]);
    expect(select.value).toBe("high");
    const save = page.target.querySelector("#model-form button[type=submit]") as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    page.pick("claude-opus-5");
    expect(page.reasoning()!.value).toBe("");
    expect(save.disabled).toBe(false);

    page.reasoning()!.value = "low";
    page.reasoning()!.dispatchEvent(new Event("change", { bubbles: true }));
    flushSync();
    page.pick("claude-sonnet-5");
    expect(page.reasoning()!.value).toBe("low");
  }

  it("draws a stored mode no longer offered as disabled, with a note, and keeps it for the same model", async () => {
    const page = await open("anthropic/claude-sonnet-5#max");
    try {
      const select = page.reasoning()!;
      expect(select.value).toBe("max");
      const max = Array.from(select.options).find((o) => o.value === "max")!;
      expect(max.disabled).toBe(true);
      expect(max.textContent).toContain("not offered");
      expect(page.target.textContent).toContain("runs on the model's own default");
      // Choosing the same model again changes nothing.
      page.pick("claude-sonnet-5");
      expect(page.reasoning()!.value).toBe("max");
      const save = page.target.querySelector("#model-form button[type=submit]") as HTMLButtonElement;
      expect(save.disabled).toBe(true);
    } finally {
      page.done();
    }
  });
});
