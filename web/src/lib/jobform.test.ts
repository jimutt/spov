import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api/client";
import {
  Committed,
  INHERIT,
  KINDS,
  createAndGo,
  defaultModelNote,
  defaultOptionLabel,
  suppressImplicitSubmit,
  keyNote,
  draftProblems,
  clockOptions,
  defaultClockLabel,
  emptyDraft,
  parseDuration,
  everyKindIsOffered,
  isSelected,
  modelGroups,
  refOf,
  repoKey,
  repoMatches,
  sessionPath,
  toJobRequest,
  defaultAccessLabel,
  toggleRepo,
  repoPhrase,
  runsFor,
  spokenLength,
  type JobDraft,
  type JobRequest,
  type ModelGroup,
  type ModelProvider,
  type Repo,
  repoReadNote,
  repoUnreachable,
} from "./jobform";

const repo = (platform: string, owner: string, name: string): Repo => ({
  platform,
  owner,
  name,
  archived: false,
  private: true,
});

const provider = (over: Partial<ModelProvider>): ModelProvider => ({
  provider: "anthropic",
  name: "Anthropic",
  key_env: "ANTHROPIC_API_KEY",
  have_key: true,
  models: ["anthropic/claude-sonnet-5"],
  ...over,
});

describe("the model select", () => {
  // @decision(DL-061) @decision(DL-013)
  it("keeps an unavailable provider and names the variable to set", () => {
    const groups = modelGroups([
      provider({
        provider: "groq",
        name: "Groq",
        key_env: "GROQ_API_KEY",
        have_key: false,
        models: ["groq/x"],
      }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].available).toBe(false);
    expect(groups[0].label).toContain("GROQ_API_KEY");
  });

  // Available first. Every provider stays, but a reader opening the
  // select should not pass the ones they cannot use to reach the ones
  // they can.
  it("puts available providers first and keeps catalogue order within each half", () => {
    const groups = modelGroups([
      provider({ provider: "a", have_key: false, models: ["a/1"] }),
      provider({ provider: "b", have_key: true, models: ["b/1"] }),
      provider({ provider: "c", have_key: false, models: ["c/1"] }),
      provider({ provider: "d", have_key: true, models: ["d/1"] }),
    ]);
    expect(groups.map((g) => g.provider)).toEqual(["b", "d", "a", "c"]);
  });

  // @decision(DL-061)
  it("survives the nulls the generated client declares", () => {
    expect(modelGroups(null)).toEqual([]);
    expect(modelGroups(undefined)).toEqual([]);
    expect(modelGroups([provider({ models: null, have_key: false })])).toEqual([]);
  });

  // A group with nothing under it is a heading that answers nothing,
  // unless a custom name can still be launched under it (DL-013).
  //
  // @decision(DL-061) @decision(DL-013)
  it("drops a provider with no models unless it has a key", () => {
    expect(modelGroups([provider({ models: [], have_key: false })])).toEqual([]);
    expect(modelGroups([provider({ models: [] })]).map((g) => g.provider)).toEqual(["anthropic"]);
  });

  // @decision(DL-061) @decision(DL-014)
  it("offers the reference the model field takes, verbatim", () => {
    const groups = modelGroups([
      provider({
        provider: "openrouter",
        models: ["openrouter/anthropic/claude-sonnet-5"],
      }),
    ]);
    expect(groups[0].models).toEqual(["openrouter/anthropic/claude-sonnet-5"]);
  });
});

describe("the repository picker", () => {
  it("matches on every term, in any order, across platform owner and name", () => {
    const r = repo("codefloe", "acme", "app");
    expect(repoMatches(r, "")).toBe(true);
    expect(repoMatches(r, "   ")).toBe(true);
    expect(repoMatches(r, "acme app")).toBe(true);
    expect(repoMatches(r, "app acme")).toBe(true);
    expect(repoMatches(r, "ACME")).toBe(true);
    expect(repoMatches(r, "codefloe app")).toBe(true);
    expect(repoMatches(r, "acme other")).toBe(false);
  });

  // The selection is held as RepoRefs and the key is only a key:
  // nothing parses it back, so an owner or a name containing a slash
  // cannot make the selection ambiguous.
  //
  // @decision(DL-061)
  it("selects by value, so a slash in a name is not a second separator", () => {
    const odd = repo("gh", "acme", "a/b");
    const plain = repo("gh", "acme", "app");
    let selected = toggleRepo([], refOf(odd));
    expect(isSelected(selected, refOf(odd))).toBe(true);
    expect(isSelected(selected, refOf(plain))).toBe(false);
    selected = toggleRepo(selected, refOf(plain));
    expect(selected).toHaveLength(2);
    selected = toggleRepo(selected, refOf(odd));
    expect(selected.map(repoKey)).toEqual(["gh/acme/app"]);
  });

  // refOf drops the listing's own fields. A request carrying `archived`
  // or `private` is sending the server a fact it did not ask for.
  it("sends only what a repo reference carries", () => {
    expect(refOf(repo("gh", "acme", "app"))).toEqual({
      platform: "gh",
      owner: "acme",
      name: "app",
    });
  });
});

describe("the draft", () => {
  const ready = (): JobDraft => ({
    ...emptyDraft(),
    goal: "  fix the flaky test  ",
    repos: [{ platform: "gh", owner: "acme", name: "app" }],
  });

  // Both kinds spend a sandbox, but only `pr` pushes a branch and opens
  // a pull request on a real forge. The higher-blast-radius kind is the
  // one a reader has to choose, not the one they get by not looking.
  //
  // @decision(DL-061)
  it("defaults to the kind that writes nothing outside the sandbox", () => {
    expect(emptyDraft().kind).toBe("report");
  });

  it("says what is stopping a submit, in the form's own terms", () => {
    expect(draftProblems(emptyDraft())).toEqual([
      "a job needs a goal",
      "choose at least one repository",
    ]);
    expect(draftProblems({ ...emptyDraft(), goal: "   " })).toContain(
      "a job needs a goal",
    );
    expect(draftProblems(ready())).toEqual([]);
  });

  // @decision(DL-061) @decision(DL-014)
  it("omits an empty model and an empty check rather than sending them", () => {
    const body = toJobRequest(ready());
    expect("model" in body).toBe(false);
    expect("check" in body).toBe(false);
    expect(body.goal).toBe("fix the flaky test");
    expect(body.kind).toBe("report");
    expect(body.repos).toEqual([
      { platform: "gh", owner: "acme", name: "app" },
    ]);
  });

  // @decision(DL-043)
  it("sends git access only when one was chosen, and labels what nothing means", () => {
    expect("git_access" in toJobRequest(ready())).toBe(false);
    expect(toJobRequest({ ...ready(), access: "all" }).git_access).toBe("all");
    expect(defaultAccessLabel("all", true)).toContain("job default");
    expect(defaultAccessLabel("all", false)).toBe("Your job default");
    expect(defaultAccessLabel(undefined, true)).toBe("This job's repositories (your job default)");
  });

  // @decision(DL-049)
  it("sends a wall clock only when one was chosen", () => {
    expect("max_duration" in toJobRequest(ready())).toBe(false);
    expect(toJobRequest({ ...ready(), clock: "2h" }).max_duration).toBe("2h");
  });

  it("trims and carries a model and a check that were given", () => {
    const body = toJobRequest({
      ...ready(),
      model: "  anthropic/claude-sonnet-5  ",
      check: "  ./spov/scripts/check.sh  ",
    });
    expect(body.model).toBe("anthropic/claude-sonnet-5");
    expect(body.check).toBe("./spov/scripts/check.sh");
  });

  // The form checks only what it knows. The size limits, the model's
  // validity and the quota are the server's, and a second copy of a
  // bound is a second thing to keep in step.
  //
  // @decision(DL-061) @decision(DL-013)
  it("does not re-state a bound the server owns", () => {
    const huge = {
      ...ready(),
      goal: "g".repeat(64 << 10),
      model: "nosuch/model",
    };
    expect(draftProblems(huge)).toEqual([]);
  });

  // A kind the API gained and the form does not offer is a job nobody
  // can start from the browser. `satisfies` catches an invented kind;
  // this constant is the other direction, and tsc is what enforces it.
  //
  // @decision(DL-061)
  it("offers every kind the API declares", () => {
    expect(everyKindIsOffered).toBe(true);
    expect([...KINDS]).toEqual(["pr", "report"]);
  });
});

describe("where a created job goes", () => {
  // @decision(DL-061) @decision(DL-055)
  it("is the session page, by query string", () => {
    expect(sessionPath("01JABCDEF")).toBe("/session/?id=01JABCDEF");
  });

  it("encodes an id rather than letting it shape the URL", () => {
    expect(sessionPath("a&b=c")).toBe("/session/?id=a%26b%3Dc");
    expect(sessionPath("../tokens/")).toBe("/session/?id=..%2Ftokens%2F");
  });
});

// @decision(DL-121)
describe("a job's sandbox template", () => {
  const base = (): JobDraft => ({
    ...emptyDraft(),
    goal: "g",
    repos: [{ platform: "gh", owner: "acme", name: "app" }],
  });

  it("opens on None and sends no template", () => {
    expect(emptyDraft().template).toBe("");
    expect("template" in toJobRequest(base())).toBe(false);
  });

  it("is sent when chosen, beside a control the person set", () => {
    expect(toJobRequest({ ...base(), template: "acme", access: "repos" })).toMatchObject({
      template: "acme",
      git_access: "repos",
    });
    // Unset, the control sends nothing, so the template's value stands.
    expect("git_access" in toJobRequest({ ...base(), template: "acme" })).toBe(false);
  });
});

describe("submitting", () => {
  const ready = (): JobDraft => ({
    ...emptyDraft(),
    goal: "fix the flaky test",
    repos: [{ platform: "gh", owner: "acme", name: "app" }],
  });

  // The body parameter is declared so `post.mock.calls[0][0]` is typed:
  // a bare `vi.fn(async () => ...)` has no parameters, and tsc reads
  // its calls as an empty tuple. vitest passes either way; astro check
  // is the step that knows.
  const ok = (sessionID?: string) =>
    vi.fn(async (_body: JobRequest) => ({
      response: { ok: true, status: 202 },
      data: { session_id: sessionID },
    }));

  // @decision(DL-061)
  it("refuses to be submitted by Enter in a single-line field", () => {
    const enter = { key: "Enter", preventDefault: vi.fn() };
    suppressImplicitSubmit(enter);
    expect(enter.preventDefault).toHaveBeenCalled();

    // Everything else still types.
    for (const key of ["a", " ", "Tab", "Backspace", "ArrowDown"]) {
      const other = { key, preventDefault: vi.fn() };
      suppressImplicitSubmit(other);
      expect(other.preventDefault, key).not.toHaveBeenCalled();
    }
  });

  // @decision(DL-061)
  it("sends the draft and leaves for the session it created", async () => {
    const post = ok("01JSESSION");
    const go = vi.fn();
    await createAndGo(ready(), post, go);

    const body = post.mock.calls[0][0];
    expect(body.goal).toBe("fix the flaky test");
    expect(body.repos).toEqual([
      { platform: "gh", owner: "acme", name: "app" },
    ]);
    expect(go).toHaveBeenCalledWith("/session/?id=01JSESSION");
  });

  it("throws the server's refusal, which a caller may retry", async () => {
    const go = vi.fn();
    const post = vi.fn(async (_body: JobRequest) => ({
      response: { ok: false, status: 422 },
      error: { title: "Unprocessable Entity", detail: "no such model" },
    }));
    await expect(createAndGo(ready(), post, go)).rejects.toBeInstanceOf(
      ApiError,
    );
    expect(go).not.toHaveBeenCalled();
  });

  // @decision(DL-061)
  it("reports a job it could not follow as committed, never as a refusal", async () => {
    const go = vi.fn();
    // Accepted, but the body carried no session to open.
    await expect(
      createAndGo(ready(), ok(undefined), go),
    ).rejects.toBeInstanceOf(Committed);
    expect(go).not.toHaveBeenCalled();

    // Accepted, and the redirect itself threw.
    const threw = vi.fn(() => {
      throw new Error("navigation is not implemented");
    });
    await expect(
      createAndGo(ready(), ok("01JSESSION"), threw),
    ).rejects.toBeInstanceOf(Committed);
  });

  it("says the job exists, so a reader does not start another", async () => {
    await expect(createAndGo(ready(), ok(undefined), vi.fn())).rejects.toThrow(
      /was created.*rather than starting another/s,
    );
  });
});

describe("why a model cannot be picked", () => {
  const keyed = (id: string, name: string, have: boolean) =>
    provider({
      provider: id,
      name,
      key_env: `${id.toUpperCase()}_API_KEY`,
      have_key: have,
      models: [`${id}/m`],
    });

  // @decision(DL-061) @decision(DL-013)
  it("is prose beside the select, not only an attribute on it", () => {
    const note = keyNote(
      modelGroups([
        keyed("openrouter", "OpenRouter", true),
        keyed("groq", "Groq", false),
      ]),
    );
    expect(note).toContain("OpenRouter");
    expect(note).toContain("Groq");
  });

  // @decision(DL-061)
  it("stays one sentence when seven of eight providers have no key", () => {
    const note = keyNote(
      modelGroups([
        keyed("openrouter", "OpenRouter", true),
        ...[
          "anthropic",
          "openai",
          "google",
          "groq",
          "xai",
          "deepseek",
          "mistral",
        ].map((id) => keyed(id, id, false)),
      ]),
    );
    expect(note.split(/[.!?](\s|$)/).filter((p) => p.trim()).length).toBe(1);
    expect(note).not.toMatch(/_API_KEY/);
    expect(note.length).toBeLessThan(180);
    expect(note).toMatch(/^OpenRouter has a key;/);
  });

  it("agrees its verbs with the counts on both sides", () => {
    const one = keyNote(
      modelGroups([keyed("a", "A", true), keyed("b", "B", false)]),
    );
    expect(one).toBe(
      "A has a key; B does not, so it is listed but cannot be chosen.",
    );
    const many = keyNote(
      modelGroups([
        keyed("a", "A", true),
        keyed("b", "B", true),
        keyed("c", "C", false),
        keyed("d", "D", false),
      ]),
    );
    expect(many).toBe(
      "A and B have a key; C and D do not, so they are listed but cannot be chosen.",
    );
  });

  // A deployment with no keys at all: the select offers only OpenCode's
  // default, and saying which providers are missing would be the whole
  // catalogue for no purpose.
  it("says what is left when nothing has a key", () => {
    const note = keyNote(
      modelGroups([keyed("a", "A", false), keyed("b", "B", false)]),
    );
    expect(note).toBe(
      "No provider has a key, so only OpenCode's own default can run.",
    );
  });

  it("says nothing when everything is available", () => {
    expect(keyNote(modelGroups([provider({ have_key: true })]))).toBe("");
    expect(keyNote([])).toBe("");
  });

  // A provider the OpenCode sign-in would unlock points at the sign-in,
  // which is a button in Settings rather than a variable to set.
  // @decision(DL-015)
  it("points at the OpenCode sign-in for the providers it would unlock", () => {
    const opencode = (id: string, name: string, have: boolean) =>
      provider({
        provider: id,
        name,
        key_env: "OPENCODE_API_KEY",
        have_key: have,
        sign_in: "opencode",
        models: [`${id}/m`],
      });
    const groups = modelGroups([
      keyed("anthropic", "Anthropic", true),
      opencode("opencode-go", "OpenCode Go", false),
      opencode("opencode", "OpenCode Zen", false),
      keyed("groq", "Groq", false),
    ]);
    expect(groups.find((g) => g.provider === "opencode-go")?.label).toBe(
      "OpenCode Go (connect OpenCode or set OPENCODE_API_KEY)",
    );
    expect(groups.find((g) => g.provider === "groq")?.label).toBe(
      "Groq (needs GROQ_API_KEY)",
    );
    expect(keyNote(groups)).toBe(
      "Anthropic has a key; OpenCode Go, OpenCode Zen and Groq do not, so they are listed but cannot be chosen." +
        " Connect OpenCode in Settings to use OpenCode Go and OpenCode Zen.",
    );
    expect(keyNote(modelGroups([opencode("opencode-go", "OpenCode Go", false)]))).toBe(
      "No provider has a key, so only OpenCode's own default can run." +
        " Connect OpenCode in Settings to use OpenCode Go.",
    );
    // Connected, or on a server that cannot give the sign-in to a
    // session: nothing to point at.
    expect(
      keyNote(modelGroups([keyed("groq", "Groq", false), opencode("opencode-go", "OpenCode Go", true)])),
    ).not.toContain("Connect OpenCode");
    expect(
      keyNote(modelGroups([keyed("a", "A", true), keyed("opencode-go", "OpenCode Go", false)])),
    ).not.toContain("Connect OpenCode");
  });
});

describe("the model select's first option", () => {
  // The near-miss DL-061 was written to prevent, asserted as the
  // PAIRING rather than as two separate facts: the option sends
  // nothing, the server resolves nothing to the account default, so an
  // option saying "OpenCode's default" on an account that has one would
  // send that default and say otherwise.
  it("sends nothing, so its label must name whatever the account resolves to", () => {
    const draft: JobDraft = { ...emptyDraft(), goal: "g", repos: [], model: INHERIT };
    expect(Object.hasOwn(toJobRequest(draft), "model")).toBe(false);
    expect(defaultOptionLabel("anthropic/claude-sonnet-5", true)).toContain(
      "anthropic/claude-sonnet-5",
    );
    expect(defaultOptionLabel("anthropic/claude-sonnet-5", true)).not.toContain("OpenCode");
  });

  it("says OpenCode's default only when the account has none", () => {
    expect(defaultOptionLabel("", true)).toBe("OpenCode's default");
  });

  // Both settled labels are claims about the account, so neither may be
  // rendered before /me has answered: the form would flash a statement
  // that may be false on every load.
  it("claims nothing about the account before /me answers", () => {
    const waiting = "Whatever your account resolves to";
    // True on BOTH branches: it must not say the reader has a default
    // (they may not) nor name one (it is not known yet).
    expect(defaultOptionLabel("", false)).toBe(waiting);
    expect(defaultOptionLabel("anthropic/claude-sonnet-5", false)).toBe(waiting);
    expect(defaultOptionLabel("anthropic/claude-sonnet-5", false)).not.toContain(
      "anthropic/claude-sonnet-5",
    );
  });

  it("trims", () => {
    expect(defaultOptionLabel("  anthropic/claude-opus-5 ", true)).toBe(
      "Your default (anthropic/claude-opus-5)",
    );
    expect(defaultOptionLabel("   ", true)).toBe("OpenCode's default");
  });
});

describe("defaultModelNote", () => {
  const usable: ModelGroup[] = [
    {
      provider: "anthropic",
      name: "Anthropic",
      label: "Anthropic",
      available: true,
      models: ["anthropic/claude-sonnet-5"],
    },
  ];
  const keyless: ModelGroup[] = [
    {
      provider: "groq",
      name: "Groq",
      label: "Groq (needs GROQ_API_KEY)",
      available: false,
      models: ["groq/llama-4-scout"],
    },
  ];

  // A value a reader is about to ACCEPT and one they CHOSE look
  // identical in a select, and only one of them follows them to every
  // other launch.
  it("names the model and says the choice is per-job", () => {
    const note = defaultModelNote("anthropic/claude-sonnet-5", true, usable);
    expect(note).toContain("anthropic/claude-sonnet-5");
    expect(note).toContain("this job only");
    expect(note).toContain("Settings");
  });

  it("is silent with no default, and before /me answers", () => {
    expect(defaultModelNote("", true, usable)).toBe("");
    expect(defaultModelNote("anthropic/claude-sonnet-5", false, usable)).toBe("");
  });

  // The note sits directly beside keyNote. Without this branch the two
  // contradict each other in adjacent sentences — "it runs on
  // groq/llama-4-scout" against "Groq does not have a key, so it is
  // listed but cannot be chosen" — while DL-014 refuses the create.
  it("says a default this deployment cannot run will be refused", () => {
    const note = defaultModelNote("groq/llama-4-scout", true, keyless);
    expect(note).toContain("cannot run it");
    expect(note).toContain("refused");
    expect(note).not.toContain("Leaving this alone runs the job");
  });

  it("says the same of a default the catalogue does not carry at all", () => {
    expect(defaultModelNote("openrouter/qwen/retired", true, usable)).toContain("refused");
  });

  // A launchable #variant default is listed under its plain reference,
  // so a literal comparison would warn about a model that works.
  it("does not warn about a #variant of a model that is available", () => {
    const note = defaultModelNote("anthropic/claude-sonnet-5#thinking", true, usable);
    expect(note).toContain("Leaving this alone runs the job");
    expect(note).not.toContain("refused");
  });

  // An empty catalogue is "not known yet", never "nothing is
  // launchable": judging from a list that did not arrive would warn
  // about a perfectly good default on every failed /models.
  it("does not warn when the model list has not arrived", () => {
    expect(defaultModelNote("anthropic/claude-sonnet-5", true, [])).toContain(
      "Leaving this alone runs the job",
    );
  });

  it("trims, and names exactly what the label names", () => {
    const padded = "  anthropic/claude-sonnet-5\n";
    expect(defaultModelNote(padded, true, usable)).toContain("anthropic/claude-sonnet-5,");
    // Pinned to each other rather than each to a literal: they trim
    // independently, so nothing else would catch the label saying `X`
    // while the note said `  X `.
    const label = defaultOptionLabel(padded, true);
    const named = label.slice(label.indexOf("(") + 1, -1);
    expect(defaultModelNote(padded, true, usable)).toContain(named);
  });
});

// @decision(DL-049)
describe("the wall clock choice", () => {
  it("reads the Go durations the API reports", () => {
    expect(parseDuration("1h0m0s")).toBe(60);
    expect(parseDuration("45m")).toBe(45);
    expect(parseDuration("2h30m")).toBe(150);
    expect(parseDuration("90s")).toBe(1.5);
    expect(parseDuration("")).toBeNull();
    expect(parseDuration("an hour")).toBeNull();
  });

  it("offers only what the limit allows", () => {
    expect(clockOptions("4h0m0s")).toEqual(["30m", "2h", "3h"]);
    expect(clockOptions("2h0m0s")).toEqual(["30m", "2h"]);
    expect(clockOptions("20m0s")).toEqual([]);
    // Unknown: offer them all and let the server refuse.
    expect(clockOptions(undefined)).toEqual(["30m", "2h", "3h"]);
    expect(clockOptions("garbage")).toEqual(["30m", "2h", "3h"]);
  });

  it("names the default, or the limit when that is lower", () => {
    expect(defaultClockLabel("4h0m0s")).toBe("1 hour (the default)");
    expect(defaultClockLabel(undefined)).toBe("1 hour (the default)");
    expect(defaultClockLabel("20m0s")).toBe("Your limit (20m)");
  });

  // @decision(DL-049)
  it("the default option names the caller's own default, within their limit", () => {
    expect(defaultClockLabel("4h0m0s", "2h0m0s")).toBe("Your default (2h)");
    expect(defaultClockLabel("4h0m0s", "1h30m0s")).toBe("Your default (1h 30m)");
    expect(defaultClockLabel("2h0m0s", "3h0m0s")).toBe("Your limit (2h)");
    expect(defaultClockLabel("4h0m0s", "")).toBe("1 hour (the default)");
  });
});

// @decision(DL-056)
describe("the submit bar", () => {
  it("says how long the job may run, as the clock select would", () => {
    expect(runsFor("2h", "4h0m0s")).toBe(120);
    expect(runsFor("", "4h0m0s")).toBe(60);
    expect(runsFor("", "4h0m0s", "1h30m0s")).toBe(90);
    expect(runsFor("", "20m0s")).toBe(20);
    expect(runsFor("", undefined)).toBe(60);
  });

  it("says a length in words", () => {
    expect(spokenLength(60)).toBe("1 hour");
    expect(spokenLength(120)).toBe("2 hours");
    expect(spokenLength(30)).toBe("30 minutes");
    expect(spokenLength(1)).toBe("1 minute");
    expect(spokenLength(90)).toBe("1 hour 30 minutes");
  });

  it("names one or two repositories and counts more", () => {
    const r = (name: string) => ({ platform: "github", owner: "acme", name });
    expect(repoPhrase([r("app")])).toBe("acme/app");
    expect(repoPhrase([r("app"), r("web")])).toBe("acme/app and acme/web");
    expect(repoPhrase([r("app"), r("web"), r("infra")])).toBe("3 repositories");
  });
});

// @decision(DL-042)
describe("a repository the browser's credential cannot reach", () => {
  const repo = (via?: ("user" | "org")[]) => ({ platform: "gh", owner: "o", name: "r", private: false, archived: false, via });
  it("is told why, and one either tier reaches is not", () => {
    const platforms = [{ platform: "gh", tier: "user" as const }];
    expect(repoUnreachable(repo(["org"]), platforms)).toMatch(/Only the organisation's credential reaches this.*your own credential/);
    expect(repoUnreachable(repo(["user", "org"]), platforms)).toBe("");
    expect(repoUnreachable(repo(undefined), platforms)).toBe("");
    expect(repoUnreachable(repo(["org"]), [{ platform: "gh", tier: "org" as const }])).toBe("");
    expect(repoUnreachable(repo(["org"]), [{ platform: "gh", tier: "none" as const, reason: "your stored credential could not be read" }])).toBe(
      "your stored credential could not be read",
    );
  });
});

// @decision(DL-042)
describe("a public repository a fine-grained token lists", () => {
  const repo = { platform: "gh", owner: "o", name: "r", private: false, archived: false, via: ["user" as const, "org" as const], maybe_read_only: true };
  it("says the token may only read it when the browser's launch uses that token", () => {
    expect(repoReadNote(repo, [{ platform: "gh", tier: "user" as const }])).toMatch(/can push only if you gave it this repository/);
    expect(repoReadNote(repo, [{ platform: "gh", tier: "org" as const }])).toBe("");
    expect(repoReadNote({ ...repo, maybe_read_only: false }, [{ platform: "gh", tier: "user" as const }])).toBe("");
  });
});
