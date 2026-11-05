import { describe, expect, it } from "vitest";
import { appProblems, checkLine, eventLine, missingLine, sourceLine, toAppBody, type OrgGitHubView } from "./orgsettings";

const view = (over: Partial<OrgGitHubView> = {}): OrgGitHubView => ({ platform: "gh", source: "stored", ...over }) as OrgGitHubView;

// @decision(DL-042)
describe("the organisation's App, in words", () => {
  it("says where it comes from", () => {
    expect(sourceLine(view({ source: "environment" }))).toMatch(/environment/);
    expect(sourceLine(view({ updated_by: "a@x" }))).toBe("Stored here by a@x.");
    expect(sourceLine(view({ source: "none" }))).toMatch(/No App/);
  });

  it("says what the check found", () => {
    expect(checkLine(view())).toBeNull();
    expect(checkLine(view({ check: { at: "", repo_count: 1, account: "acme" } }))).toEqual({
      ok: true,
      text: "Installed on acme, reaching 1 repository.",
    });
    expect(checkLine(view({ check: { at: "", repo_count: 0, error: "no installation" } }))?.ok).toBe(false);
    expect(missingLine(view({ check: { at: "", repo_count: 3, missing: ["issues:write"] } }))).toBe(
      "It lacks issues write, which sandboxes ask for.",
    );
    expect(missingLine(view())).toBe("");
  });

  it("names each audited change", () => {
    expect(eventLine({ actor: "a@x", action: "github_app.stored", detail: "App 1 on acme", at: "" })).toBe("a@x stored App 1 on acme");
  });
});

// @decision(DL-062) @decision(DL-042)
describe("storing an App", () => {
  it("needs an ID and a PEM key, and sends the account only when given", () => {
    expect(appProblems({ appID: "", account: "", key: "" })).toHaveLength(2);
    expect(appProblems({ appID: "1", account: "", key: "nope" })).toEqual(["the private key is the PEM file GitHub gave you"]);
    expect(appProblems({ appID: "1", account: "", key: "-----BEGIN RSA PRIVATE KEY-----" })).toEqual([]);
    expect(toAppBody({ appID: " 1 ", account: "", key: " k " })).toEqual({ app_id: "1", private_key: "k" });
    expect(toAppBody({ appID: "1", account: "acme", key: "k" }).account).toBe("acme");
  });
});
