import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api/client";
import {
  builtInDurationLabel,
  jobDurationOptions,
  jobDurationValue,
  saveJobDuration,
  savedJobDurationNote,
  credentialFacts,
  forgeTokenAdvice,
  ALL_ON_ORG_NOTE,
  accessValue,
  gitAccessBody,
  saveGitAccess,
  savedAccessNote,
  NO_DEFAULT,
  defaultModelBody,
  keyNote,
  modelGroups,
  saveDefaultModel,
  savedNote,
  changed,
  emptyField,
  identityBody,
  saveIdentity,
  savedIdentityNote,
  seedField,
  withoutVariant,
  type Field,
  type Me,
  type ModelProvider,
} from "./settings";

const provider = (
  id: string,
  name: string,
  keyEnv: string,
  haveKey: boolean,
  models: string[],
): ModelProvider => ({
  provider: id,
  name,
  key_env: keyEnv,
  have_key: haveKey,
  models,
});

const anthropic = provider("anthropic", "Anthropic", "ANTHROPIC_API_KEY", true, [
  "anthropic/claude-sonnet-5",
  "anthropic/claude-opus-5",
]);
const groq = provider("groq", "Groq", "GROQ_API_KEY", false, [
  "groq/llama-4-scout",
]);

const ok = (me: Me) => ({ response: new Response(null, { status: 200 }), data: me });

describe("withoutVariant", () => {
  it("splits on the FIRST hash, as the server does", () => {
    expect(withoutVariant("anthropic/claude-sonnet-5#high")).toBe("anthropic/claude-sonnet-5");
    // strings.Cut takes everything after the first # as the variant, so
    // this resolves to model `b` and must trim to the same place.
    expect(withoutVariant("a/b#x#y")).toBe("a/b");
  });

  it("leaves a reference with an empty half alone", () => {
    // The server refuses both of these, so trimming them into something
    // that resolves would report an unusable value as launchable.
    expect(withoutVariant("anthropic/claude-sonnet-5#")).toBe("anthropic/claude-sonnet-5#");
    expect(withoutVariant("#high")).toBe("#high");
  });

  it("leaves a plain reference alone", () => {
    expect(withoutVariant("anthropic/claude-sonnet-5")).toBe("anthropic/claude-sonnet-5");
  });
});

describe("seedField", () => {
  const field = (stored: string | undefined, value: string): Field => ({ stored, value });

  // The effect runs once before the request lands. Without the guard it
  // records "" as the stored value, the arriving profile is no longer
  // the first, and the reader's own setting never reaches the control.
  it("does nothing until the profile has loaded", () => {
    expect(seedField(false, "", emptyField())).toEqual(emptyField());
    const after = seedField(true, "anthropic/claude-sonnet-5", emptyField());
    expect(after).toEqual(field("anthropic/claude-sonnet-5", "anthropic/claude-sonnet-5"));
  });

  it("leaves a choice the reader has made", () => {
    const chosen = field("anthropic/claude-sonnet-5", "anthropic/claude-opus-5");
    expect(seedField(true, "anthropic/claude-sonnet-5", chosen)).toEqual(chosen);
  });

  // Testing `stored === undefined` alone is a one-shot: the resource
  // reloads whenever the tab becomes visible, so a value changed
  // elsewhere would leave this control stale with Save armed to revert
  // it.
  it("follows a value changed elsewhere while the reader has not diverged", () => {
    const untouched = field("anthropic/claude-sonnet-5", "anthropic/claude-sonnet-5");
    expect(seedField(true, "groq/llama-4-scout", untouched)).toEqual(
      field("groq/llama-4-scout", "groq/llama-4-scout"),
    );
  });

  it("records a cleared value as cleared rather than as unknown", () => {
    const after = seedField(true, NO_DEFAULT, emptyField());
    expect(after.stored).toBe(NO_DEFAULT);
    expect(after.stored).not.toBeUndefined();
  });

  // The unit-level half of the loop guard: the caller is an $effect
  // that writes this back, so a pass that changed nothing must return
  // the SAME reference or Svelte re-dirties the effect's own
  // dependency. settings.svelte.test.ts holds the other half.
  it("returns the very same object when nothing moved", () => {
    const settled = field("Ada", "Ada");
    expect(seedField(true, "Ada", settled)).toBe(settled);
    const diverged = field("Ada", "Grace");
    expect(seedField(true, "Ada", diverged)).toBe(diverged);
  });
});

describe("changed", () => {
  it("is false while nothing has been read", () => {
    expect(changed(emptyField(), emptyField())).toBe(false);
  });

  it("is true when any one field diverges", () => {
    const same: Field = { stored: "Ada", value: "Ada" };
    const moved: Field = { stored: "ada@example.com", value: "ada@lovelace.test" };
    expect(changed(same, same)).toBe(false);
    expect(changed(same, moved)).toBe(true);
    expect(changed(moved, same)).toBe(true);
  });

  // A save trims, so an untrimmed comparison arms the button for a
  // request the server no-ops — and its answer then silently reverts
  // the input the reader is looking at.
  it("ignores whitespace a save would strip anyway", () => {
    expect(changed({ stored: "Ada", value: "  Ada " })).toBe(false);
    expect(changed({ stored: "Ada", value: "  Ada B " })).toBe(true);
  });

  // Nothing can differ from a value that was never read, so a field
  // still waiting on the profile must not arm a save that would then
  // write an empty string over it.
  it("ignores a field the profile has not reached", () => {
    expect(changed({ stored: undefined, value: "typed anyway" })).toBe(false);
  });
});

describe("identityBody", () => {
  it("trims both halves", () => {
    expect(identityBody("  Ada ", " ada@example.com\n")).toEqual({
      git_name: "Ada",
      git_email: "ada@example.com",
    });
  });

  // The same rule as defaultModelBody and for the same reason: these
  // fields ARE the setting, so an omitted one would mean "leave it
  // alone" and clearing would be impossible.
  it("SENDS both halves even when empty, so clearing works", () => {
    const body = identityBody("", "");
    expect(Object.hasOwn(body, "git_name")).toBe(true);
    expect(Object.hasOwn(body, "git_email")).toBe(true);
    expect(body).toEqual({ git_name: "", git_email: "" });
  });
});

describe("saveIdentity", () => {
  it("answers with the profile the server now holds", async () => {
    const patch = vi.fn(async (_body: { git_name: string; git_email: string }) =>
      ok({ git_name: "Ada", git_email: "ada@example.com" } as Me),
    );
    const me = await saveIdentity(" Ada ", "ada@example.com", patch);
    expect(me.git_name).toBe("Ada");
    expect(patch.mock.calls[0][0]).toEqual({
      git_name: "Ada",
      git_email: "ada@example.com",
    });
  });

  it("throws the server's refusal rather than reporting a save", async () => {
    const patch = vi.fn(async (_body: { git_name: string; git_email: string }) => ({
      response: new Response(null, { status: 501 }),
      error: { type: "about:blank", detail: "this server has no user profiles to update" },
    }));
    await expect(saveIdentity("Ada", "a@b.test", patch)).rejects.toBeInstanceOf(ApiError);
  });

  it("does not claim a save when the answer carries no profile", async () => {
    const patch = vi.fn(async (_body: { git_name: string; git_email: string }) => ({
      response: new Response(null, { status: 200 }),
    }));
    await expect(saveIdentity("Ada", "a@b.test", patch)).rejects.toThrow(/cannot show/);
  });
});

describe("savedIdentityNote", () => {
  it("names the author commits will carry", () => {
    expect(savedIdentityNote({ git_name: "Ada", git_email: "ada@example.com" } as Me)).toBe(
      "Sessions will author commits as Ada <ada@example.com>.",
    );
  });

  it("says a clear happened rather than saying nothing", () => {
    expect(savedIdentityNote({} as Me)).toContain("server's own identity");
  });

  // git.ResolveIdentity layers the request, then the profile, then the
  // server's configured identity FIELD BY FIELD, so half an identity is
  // not half a fallback — it is a MIXED author, this name beside the
  // server's e-mail, which is the kind of thing noticed in a pull
  // request rather than here.
  it("warns that half an identity produces a mixed author", () => {
    const noEmail = savedIdentityNote({ git_name: "Ada" } as Me);
    expect(noEmail).toContain("your name beside the server's e-mail address");
    expect(noEmail).toContain("Set both, or clear both");

    const noName = savedIdentityNote({ git_email: "ada@example.com" } as Me);
    expect(noName).toContain("your e-mail address beside the server's name");
  });

  it("treats a whitespace-only half as absent", () => {
    expect(savedIdentityNote({ git_name: "  ", git_email: "  " } as Me)).toContain(
      "server's own identity",
    );
  });
});

describe("keyNote", () => {
  // Reused from the job form rather than re-implemented: it is the same
  // question on a second screen, and the copy that drifted would be
  // this one.
  it("is the job form's, applied to the settings groups", () => {
    const note = keyNote(modelGroups([anthropic, groq]));
    expect(note).toBe(
      "Anthropic has a key; Groq does not, so it is listed but cannot be chosen.",
    );
  });

  // On this page the sign-in is the section below, not another page.
  // @decision(DL-015)
  it("points down the page at the OpenCode sign-in", () => {
    const go = {
      provider: "opencode-go",
      name: "OpenCode Go",
      key_env: "OPENCODE_API_KEY",
      have_key: false,
      sign_in: "opencode" as const,
      models: ["opencode-go/kimi-k3"],
    };
    expect(keyNote(modelGroups([anthropic, go]))).toMatch(
      / Connect OpenCode below to use OpenCode Go\.$/,
    );
  });
});

describe("defaultModelBody", () => {
  it("carries the model", () => {
    expect(defaultModelBody("anthropic/claude-sonnet-5")).toEqual({
      default_model: "anthropic/claude-sonnet-5",
    });
  });

  it("trims", () => {
    expect(defaultModelBody("  anthropic/claude-sonnet-5 ")).toEqual({
      default_model: "anthropic/claude-sonnet-5",
    });
  });

  // The opposite of the job form's rule, and deliberately. There,
  // omitting the model means "resolve it"; here the field IS the
  // setting, so an omitted one means "leave my default alone" and there
  // would be no way to clear it. Making the two consistent is exactly
  // the tidy-up that removes the clear.
  it("SENDS an empty model rather than omitting it, so clearing works", () => {
    const body = defaultModelBody(NO_DEFAULT);
    expect(Object.hasOwn(body, "default_model")).toBe(true);
    expect(body.default_model).toBe("");
  });
});

describe("saveDefaultModel", () => {
  it("answers with the profile the server now holds", async () => {
    const patch = vi.fn(async (_body: { default_model: string }) =>
      ok({ user_id: "u", org_id: "o", admin: false, default_model: "anthropic/claude-opus-5" } as Me),
    );
    const me = await saveDefaultModel("anthropic/claude-opus-5", patch);
    expect(me.default_model).toBe("anthropic/claude-opus-5");
    expect(patch.mock.calls[0][0]).toEqual({ default_model: "anthropic/claude-opus-5" });
  });

  // DL-014 refuses an unusable model here, so a refusal has to arrive
  // as one rather than as a page that claims it saved.
  it("throws the server's refusal rather than reporting a save", async () => {
    const patch = vi.fn(async (_body: { default_model: string }) => ({
      response: new Response(null, { status: 422 }),
      error: { type: "about:blank", detail: "openai/nope cannot be your default model" },
    }));
    await expect(saveDefaultModel("openai/nope", patch)).rejects.toBeInstanceOf(ApiError);
  });

  it("does not claim a save when the answer carries no profile", async () => {
    const patch = vi.fn(async (_body: { default_model: string }) => ({
      response: new Response(null, { status: 200 }),
    }));
    await expect(saveDefaultModel("a/b", patch)).rejects.toThrow(/cannot show/);
  });
});

describe("savedNote", () => {
  it("names the model a launch will now use", () => {
    expect(savedNote({ default_model: "anthropic/claude-sonnet-5" } as Me)).toContain(
      "anthropic/claude-sonnet-5",
    );
  });

  // Clearing must say something. A note that went blank on a clear
  // reads as the save having failed.
  it("says a clear happened rather than saying nothing", () => {
    expect(savedNote({} as Me)).toContain("Cleared");
    expect(savedNote({ default_model: "" } as Me)).toContain("Cleared");
  });
});

describe("the advice under a forge token", () => {
  // @decision(DL-043)
  it("says every sandbox gets it, and how to scope a Forgejo one", () => {
    expect(forgeTokenAdvice("forgejo:codefloe.com")).toContain("Specific repositories");
    expect(forgeTokenAdvice("forgejo:codefloe.com")).toContain("cannot check");
    expect(forgeTokenAdvice("github")).toContain("cannot check");
    expect(forgeTokenAdvice("github")).not.toContain("Specific repositories");
    // @decision(DL-034)
    expect(forgeTokenAdvice("github")).toContain("fine-grained");
    expect(forgeTokenAdvice("forgejo:codefloe.com")).toContain("proxy");
  });

  // @decision(DL-041) @decision(DL-034)
  it("names the scope a Gitea or GitLab token needs, and that GitLab's reaches every project", () => {
    expect(forgeTokenAdvice("gitea:gitea.com")).toContain("write:repository and write:issue");
    // @decision(DL-041)
    expect(forgeTokenAdvice("forgejo:codefloe.com")).toContain("write:issue");
    expect(forgeTokenAdvice("gitlab:gitlab.com")).toContain("api scope");
    expect(forgeTokenAdvice("gitlab:gitlab.com")).toContain("every project");
  });

  // @decision(DL-043)
  it("says nothing for a model key or an empty field", () => {
    expect(forgeTokenAdvice("provider:ANTHROPIC_API_KEY")).toBe("");
    expect(forgeTokenAdvice("  ")).toBe("");
  });
});

// @decision(DL-043)
describe("git access defaults", () => {
  it("shows an unset default as repos, and sends both values", () => {
    expect(accessValue(undefined)).toBe("repos");
    expect(accessValue("")).toBe("repos");
    expect(accessValue("all")).toBe("all");
    expect(gitAccessBody("all", "repos")).toEqual({ default_git_access: "all", default_job_git_access: "repos" });
  });

  it("reports what the server holds, and where all may not apply", () => {
    const me = (s?: string, j?: string) => ({ default_git_access: s, default_job_git_access: j }) as unknown as Me;
    expect(savedAccessNote(me())).toBe("Sessions will reach only their own repositories, and jobs only their own repositories.");
    expect(savedAccessNote(me("all"))).toContain("every repository your credentials reach, and jobs only");
    expect(savedAccessNote(me("all"))).toContain(ALL_ON_ORG_NOTE);
  });

  it("returns the saved profile, and refuses a failure", async () => {
    const ok = vi.fn(async () => ({ response: new Response(null, { status: 200 }), data: { default_git_access: "all" } as Me }));
    expect((await saveGitAccess("all", "repos", ok)).default_git_access).toBe("all");
    expect(ok).toHaveBeenCalledWith({ default_git_access: "all", default_job_git_access: "repos" });
    const refused = async () => ({ response: new Response(null, { status: 422 }), error: { type: "about:blank", title: "no" } });
    await expect(saveGitAccess("all", "repos", refused)).rejects.toBeInstanceOf(ApiError);
  });
});

// @decision(DL-049)
describe("the default job duration", () => {
  it("seeds the select from the stored duration", () => {
    expect(jobDurationValue(undefined)).toBe("");
    expect(jobDurationValue("2h0m0s")).toBe("2h");
    expect(jobDurationValue("30m0s")).toBe("30m");
    expect(jobDurationValue("1h30m0s")).toBe("1h30m");
    expect(jobDurationValue("45m30s")).toBe("45m30s");
  });

  it("names the built-in default as the job form does, within the limit", () => {
    expect(builtInDurationLabel("4h0m0s")).toBe("1 hour (the default)");
    expect(builtInDurationLabel("20m0s")).toBe("Your limit (20m)");
  });

  it("offers what the limit allows, and keeps a stored value above it", () => {
    expect(jobDurationOptions("4h0m0s", "")).toEqual(["30m", "1h", "2h", "3h", "4h"]);
    expect(jobDurationOptions("2h0m0s", "")).toEqual(["30m", "1h", "2h"]);
    expect(jobDurationOptions("2h0m0s", "3h")).toEqual(["30m", "1h", "2h", "3h"]);
    expect(jobDurationOptions(undefined, "1h30m")).toEqual(["30m", "1h", "2h", "3h", "4h", "1h30m"]);
  });

  it("sends the value, empty clearing it, and returns the stored profile", async () => {
    const patch = vi.fn(async () => ({
      response: new Response(null, { status: 200 }),
      data: { default_job_duration: "2h0m0s" } as never,
    }));
    const me = await saveJobDuration("2h", patch);
    expect(patch).toHaveBeenCalledWith({ default_job_duration: "2h" });
    expect(me.default_job_duration).toBe("2h0m0s");
    await saveJobDuration("", patch);
    expect(patch).toHaveBeenLastCalledWith({ default_job_duration: "" });
  });

  it("refuses what the server refused", async () => {
    const patch = async () => ({ response: new Response(null, { status: 422 }) });
    await expect(saveJobDuration("20s", patch)).rejects.toBeInstanceOf(ApiError);
  });

  it("says what jobs will get, and when the limit cuts it", () => {
    expect(savedJobDurationNote({} as never)).toContain("up to an hour");
    expect(savedJobDurationNote({ quota: { max_job_duration: "20m0s" } } as never)).toContain("Your limit is 20m");
    expect(savedJobDurationNote({ default_job_duration: "2h0m0s" } as never)).toContain("up to 2h");
    const capped = savedJobDurationNote({
      default_job_duration: "3h0m0s",
      quota: { max_job_duration: "2h0m0s" },
    } as never);
    expect(capped).toContain("Your limit is 2h");
  });
});

describe("what the list says about a stored credential", () => {
  const now = Date.parse("2026-11-01T00:00:00Z");
  // @decision(DL-041)
  it("names the account, the scopes and when it expires", () => {
    expect(
      credentialFacts({ verified_as: "bot", scopes: ["api"], expires_at: "2026-11-13T00:00:00Z" }, now),
    ).toBe("Verified as bot · scopes api · expires 2026-11-13, in 12 days");
    expect(credentialFacts({ expires_at: "2027-06-01T00:00:00Z" }, now)).toBe("expires 2027-06-01");
    expect(credentialFacts({ expires_at: "2026-10-30T00:00:00Z" }, now)).toBe("expired 2026-10-30");
    expect(credentialFacts({}, now)).toBe("");
  });
});
