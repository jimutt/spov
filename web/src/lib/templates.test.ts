import { describe, expect, it } from "vitest";
import {
  credentialRows,
  draftFromTemplate,
  emptyTemplateDraft,
  missingLine,
  templateAccessLabel,
  templateProblems,
  templateSummary,
  toTemplateBody,
  togglePicked,
  type TemplateDraft,
} from "./templates";
import type { RoutedCredential } from "./routedform";

function draft(over: Partial<TemplateDraft>): TemplateDraft {
  return { ...emptyTemplateDraft(), name: "acme", ...over };
}

// @decision(DL-121)
describe("a template's summary", () => {
  it("names what it sets and nothing it leaves out", () => {
    expect(
      templateSummary({
        egress: "install",
        credentials: ["npm", "maven"],
        env: { A: "1", B: "2", C: "3" },
        addons: ["rust", "postgres@16"],
      }),
    ).toBe("egress install, 2 credentials, 3 variables, rust and postgres@16");
    expect(templateSummary({ credentials: ["npm"] })).toBe("1 credential");
    expect(templateSummary({ no_credentials: true, git_access: "all" })).toBe("no credentials, git access to every repository");
    expect(templateSummary({ egress: "agent", egress_extra_domains: ["a.example"], apt: ["jq", "vim"], git_access: "repos" })).toBe(
      "egress agent with 1 more domain, 2 apt packages, git access to its repositories",
    );
  });

  it("is empty for a template that sets nothing", () => {
    expect(templateSummary({})).toBe("");
    expect(templateSummary({ description: "only words", credentials: null, addons: [], env: {} })).toBe("");
  });

  it("says which credentials the caller does not hold", () => {
    expect(missingLine({ name: "t" })).toBe("");
    expect(missingLine({ name: "t", missing_credentials: ["npm", "maven"] })).toContain("You do not hold npm and maven");
  });
});

// @decision(DL-121) @decision(DL-119)
describe("the request a template draft builds", () => {
  it("leaves every not-set control out of the body", () => {
    expect(toTemplateBody(draft({}))).toEqual({});
    // An untouched variable row and blank lines are not settings either.
    expect(toTemplateBody(draft({ env: [{ name: "", value: "" }], addons: " \n", apt: "\n", description: "  " }))).toEqual({});
  });

  it("sends each control that is set", () => {
    expect(
      toTemplateBody(
        draft({
          description: " Backend work ",
          egress: "install",
          domains: "a.example\n\n b.example ",
          credentials: "only",
          picked: ["npm", "maven"],
          env: [
            { name: " REGION ", value: "eu " },
            { name: "", value: "" },
          ],
          addons: "rust\npostgres@16",
          apt: "jq",
          access: "repos",
        }),
      ),
    ).toEqual({
      description: "Backend work",
      egress: "install",
      egress_extra_domains: ["a.example", "b.example"],
      credentials: ["npm", "maven"],
      env: { REGION: "eu " },
      addons: ["rust", "postgres@16"],
      apt: ["jq"],
      git_access: "repos",
    });
  });

  it("sends extra domains only with a preset that takes them", () => {
    expect(toTemplateBody(draft({ egress: "open", domains: "a.example" }))).toEqual({ egress: "open" });
    expect(toTemplateBody(draft({ egress: "", domains: "a.example" }))).toEqual({});
  });

  it("says None with no_credentials and never with a list", () => {
    // Names ticked before the choice moved to None do not ride along.
    expect(toTemplateBody(draft({ credentials: "none", picked: ["npm"] }))).toEqual({ no_credentials: true });
    expect(toTemplateBody(draft({ credentials: "default", picked: ["npm"] }))).toEqual({});
  });

  it("makes Only these with nothing ticked a problem and not a request", () => {
    const d = draft({ credentials: "only", picked: [] });
    expect(toTemplateBody(d)).toBeNull();
    expect(templateProblems(d, "add", [])).toEqual(["tick at least one credential, or choose None to attach none"]);
    expect(templateProblems({ ...d, picked: ["npm"] }, "add", [])).toEqual([]);
  });

  it("checks the name, and that an add does not overwrite", () => {
    expect(templateProblems(draft({ name: "Acme" }), "add", [])).toHaveLength(1);
    expect(templateProblems(draft({ name: "none" }), "add", [])).toEqual(["none cannot be a template's name"]);
    expect(templateProblems(draft({}), "add", ["acme"])).toEqual(["you already have acme: use Edit on its row"]);
    expect(templateProblems(draft({}), "edit", ["acme"])).toEqual([]);
  });

  it("refuses a variable with no name, or two of one name", () => {
    expect(templateProblems(draft({ env: [{ name: "", value: "x" }] }), "add", [])).toEqual(["every variable needs a name"]);
    expect(
      templateProblems(
        draft({
          env: [
            { name: "A", value: "1" },
            { name: "A", value: "2" },
          ],
        }),
        "add",
        [],
      ),
    ).toEqual(["a variable is named twice"]);
  });

  it("opens a stored template as the draft that builds it again", () => {
    const stored = {
      description: "d",
      egress: "agent",
      egress_extra_domains: ["a.example"],
      credentials: ["npm"],
      env: { B: "2", A: "1" },
      addons: ["rust"],
      apt: ["jq"],
      git_access: "all" as const,
    };
    const d = draftFromTemplate({ name: "acme", ...stored, updated_at: "2026-11-10T00:00:00Z", missing_credentials: [] });
    expect(d.name).toBe("acme");
    expect(d.credentials).toBe("only");
    expect(d.env.map((r) => r.name)).toEqual(["A", "B"]);
    expect(toTemplateBody(d)).toEqual(stored);
    expect(draftFromTemplate({ name: "n", no_credentials: true }).credentials).toBe("none");
    expect(draftFromTemplate({ name: "n" }).credentials).toBe("default");
    expect(toTemplateBody(draftFromTemplate({ name: "n" }))).toEqual({});
  });
});

// @decision(DL-121) @decision(DL-075)
describe("the credential checklist", () => {
  const rc = (name: string, host: string, attach: "always" | "asked" = "always"): RoutedCredential => ({
    route: { name, hosts: [host], scheme: "bearer", attach },
  });

  it("has one row per name, the caller's own where both hold it", () => {
    const rows = credentialRows([rc("npm", "mine.example"), rc("zeta", "z.example", "asked")], [rc("npm", "org.example"), rc("maven", "m.example")], []);
    expect(rows.map((r) => [r.name, r.owner])).toEqual([
      ["maven", "org"],
      ["npm", "me"],
      ["zeta", "me"],
    ]);
    expect(rows[1].line).toBe("mine.example, as a bearer, every session");
    expect(rows[2].line).toBe("z.example, as a bearer, only when a launch asks");
    expect(rows.every((r) => !r.checked)).toBe(true);
  });

  it("ticks the picked names and keeps one nobody holds", () => {
    const rows = credentialRows([rc("npm", "mine.example")], null, ["npm", "gone"]);
    expect(rows.map((r) => [r.name, r.owner, r.checked])).toEqual([
      ["gone", "missing", true],
      ["npm", "me", true],
    ]);
  });

  it("is empty before either list arrives", () => {
    expect(credentialRows(undefined, undefined, [])).toEqual([]);
  });

  it("toggles a name", () => {
    expect(togglePicked([], "npm")).toEqual(["npm"]);
    expect(togglePicked(["npm", "maven"], "npm")).toEqual(["maven"]);
  });
});

// @decision(DL-121) @decision(DL-120)
describe("the job form's git access label", () => {
  it("names a chosen template that sets git access, and nothing otherwise", () => {
    expect(templateAccessLabel(undefined)).toBe("");
    expect(templateAccessLabel({ name: "acme" })).toBe("");
    expect(templateAccessLabel({ name: "acme", git_access: "all" })).toBe("Every repository (the acme template)");
    expect(templateAccessLabel({ name: "acme", git_access: "repos" })).toBe("This job's repositories (the acme template)");
  });
});
