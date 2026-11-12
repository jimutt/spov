import { describe, expect, it } from "vitest";
import {
  draftFromRoute,
  emptyRouteDraft,
  listOf,
  nameProblem,
  noEnterSubmit,
  routeProblems,
  storedNote,
  toRoute,
  toRoutedBody,
  type RouteDraft,
} from "./routedform";

function draft(over: Partial<RouteDraft>): RouteDraft {
  return { ...emptyRouteDraft(), name: "reg", secret: "s3cret", ...over };
}

// @decision(DL-118)
describe("the request a routed credential draft builds", () => {
  it("sends a bearer route with its hosts and paths", () => {
    expect(toRoutedBody(draft({ hosts: "npm.acme.com, pkg.acme.com", paths: "/repo/\n/other/", scheme: "bearer" }))).toEqual({
      secret: "s3cret",
      route: { name: "reg", hosts: ["npm.acme.com", "pkg.acme.com"], paths: ["/repo/", "/other/"], scheme: "bearer", attach: "always" },
    });
  });

  it("sends a username only with Basic and a header name only with header", () => {
    const filled = { hosts: "h.example", user: "ada", header: "X-Key" };
    expect(toRoute(draft({ ...filled, scheme: "basic" }))).toEqual({
      name: "reg",
      hosts: ["h.example"],
      scheme: "basic",
      user: "ada",
      attach: "always",
    });
    expect(toRoute(draft({ ...filled, scheme: "header", attach: "asked" }))).toEqual({
      name: "reg",
      hosts: ["h.example"],
      scheme: "header",
      header: "X-Key",
      attach: "asked",
    });
    expect(toRoute(draft({ ...filled, scheme: "bearer" }))).toEqual({
      name: "reg",
      hosts: ["h.example"],
      scheme: "bearer",
      attach: "always",
    });
  });

  it("sends a preset's name and options and never a host or a scheme", () => {
    // Fields left over from a custom route must not ride along.
    const stale = { hosts: "old.example", paths: "/x/", scheme: "header" as const, header: "X-Old" };
    expect(toRoute(draft({ ...stale, kind: "npm", url: " https://npm.acme.com/ ", scope: "@acme", user: "ignored" }))).toEqual({
      name: "reg",
      preset: "npm",
      url: "https://npm.acme.com/",
      scope: "@acme",
      attach: "always",
    });
    expect(toRoute(draft({ ...stale, kind: "npm", url: "https://npm.acme.com/" }))).toEqual({
      name: "reg",
      preset: "npm",
      url: "https://npm.acme.com/",
      attach: "always",
    });
    expect(toRoute(draft({ ...stale, kind: "maven", url: "https://m.acme.com/repo", user: "ci", repoID: "central", scope: "@no" }))).toEqual({
      name: "reg",
      preset: "maven",
      url: "https://m.acme.com/repo",
      user: "ci",
      repo_id: "central",
      attach: "always",
    });
    expect(toRoute(draft({ kind: "nuget", url: "https://n.acme.com/v3/", repoID: "no" }))).toEqual({
      name: "reg",
      preset: "nuget",
      url: "https://n.acme.com/v3/",
      attach: "always",
    });
    expect(toRoute(draft({ kind: "pypi", url: "https://p.acme.com/simple/", user: "me", attach: "asked" }))).toEqual({
      name: "reg",
      preset: "pypi",
      url: "https://p.acme.com/simple/",
      user: "me",
      attach: "asked",
    });
  });

  it("sends the secret as typed", () => {
    expect(toRoutedBody(draft({ hosts: "h.example", scheme: "bearer", secret: " padded " })).secret).toBe(" padded ");
  });
});

// @decision(DL-118)
describe("the local checks", () => {
  const good = draft({ hosts: "h.example", scheme: "bearer" });

  it("pass a complete draft", () => {
    expect(routeProblems(good, "add", [])).toEqual([]);
  });

  it("check the name's shape", () => {
    expect(nameProblem("")).toBe("give it a name");
    for (const bad of ["Reg", "1reg", "-reg", "re_g", "re.g", "..", "a".repeat(41)]) {
      expect(nameProblem(bad), bad).toContain("lowercase letters");
    }
    for (const ok of ["a", "reg-2", "a".repeat(40)]) expect(nameProblem(ok), ok).toBe("");
    expect(routeProblems({ ...good, name: ".." }, "replace", [])).toHaveLength(1);
  });

  it("need a host, a scheme and a secret", () => {
    expect(routeProblems({ ...good, hosts: " , " }, "add", [])).toEqual(["name at least one host"]);
    expect(routeProblems({ ...good, scheme: "" }, "add", [])).toEqual(["choose how the secret is sent"]);
    expect(routeProblems({ ...good, secret: "" }, "add", [])).toEqual(["type the secret"]);
    expect(routeProblems({ ...good, scheme: "basic" }, "add", [])).toEqual(["Basic needs a username"]);
    expect(routeProblems({ ...good, scheme: "header" }, "add", [])).toEqual(["name the header"]);
  });

  it("ask a preset for its address, not for a host or a scheme", () => {
    expect(routeProblems(draft({ kind: "npm" }), "add", [])).toEqual(["give the registry's address"]);
    expect(routeProblems(draft({ kind: "npm", url: "https://npm.acme.com/" }), "add", [])).toEqual([]);
    expect(routeProblems(draft({ kind: "maven", url: "https://m.acme.com/" }), "add", [])).toEqual(["Maven needs a username"]);
  });

  it("refuse to add a name the chosen owner already holds, and point to Replace", () => {
    expect(routeProblems(good, "add", ["reg"])).toEqual(["you already have reg: use Replace on its row"]);
    expect(routeProblems({ ...good, owner: "org" }, "add", ["reg"])).toEqual([
      "the organisation already has reg: use Replace on its row",
    ]);
    expect(routeProblems(good, "add", ["other"])).toEqual([]);
    // A replace is of a held name by definition.
    expect(routeProblems(good, "replace", ["reg"])).toEqual([]);
  });
});

// @decision(DL-118)
describe("a replace", () => {
  it("prefills the route and never a secret", () => {
    const d = draftFromRoute(
      { name: "reg", hosts: ["a.example", "b.example"], paths: ["/r/"], scheme: "basic", user: "ada", attach: "asked" },
      "org",
    );
    expect(d).toMatchObject({ owner: "org", name: "reg", kind: "custom", scheme: "basic", user: "ada", attach: "asked", secret: "" });
    expect(listOf(d.hosts)).toEqual(["a.example", "b.example"]);
    expect(listOf(d.paths)).toEqual(["/r/"]);
  });

  it("keeps a preset's widened path, so a replace does not narrow it", () => {
    const stored = {
      name: "team",
      preset: "npm" as const,
      url: "https://art.acme.com/api/npm/team-a/",
      hosts: ["art.acme.com"],
      paths: ["/api/npm/"],
      scheme: "bearer" as const,
    };
    const d = draftFromRoute(stored, "me");
    expect(d.widened).toEqual(["/api/npm/"]);
    expect(toRoute(d).paths).toEqual(["/api/npm/"]);
    // The address's own path is the server's to work out, and is not sent.
    const own = draftFromRoute({ ...stored, paths: ["/api/npm/team-a/"] }, "me");
    expect(own.widened).toEqual([]);
    expect(toRoute(own).paths).toBeUndefined();
  });

  it("prefills a preset from its options, leaving the server's expansion out", () => {
    const d = draftFromRoute(
      {
        name: "mvn",
        preset: "maven",
        url: "https://m.acme.com/repo/",
        hosts: ["m.acme.com"],
        paths: ["/repo/"],
        scheme: "basic",
        user: "ci",
        repo_id: "central",
        attach: "always",
      },
      "me",
    );
    expect(toRoute({ ...d, secret: "x" })).toEqual({
      name: "mvn",
      preset: "maven",
      url: "https://m.acme.com/repo/",
      user: "ci",
      repo_id: "central",
      attach: "always",
    });
  });
});

describe("the form's furniture", () => {
  it("says a stored credential is unchecked", () => {
    expect(storedNote("reg", "me")).toContain("Stored reg, unchecked");
    expect(storedNote("reg", "org")).toContain("Stored reg for the organisation, unchecked");
  });

  it("keeps Enter in a field from submitting, and leaves a textarea and a button alone", () => {
    const press = (tagName: string, key = "Enter") => {
      let stopped = false;
      noEnterSubmit({ key, target: { tagName }, preventDefault: () => (stopped = true) });
      return stopped;
    };
    expect(press("INPUT")).toBe(true);
    expect(press("SELECT")).toBe(true);
    expect(press("TEXTAREA")).toBe(false);
    expect(press("BUTTON")).toBe(false);
    expect(press("INPUT", "a")).toBe(false);
  });
});
