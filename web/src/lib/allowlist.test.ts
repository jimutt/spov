import { describe, expect, it } from "vitest";
import { ApiError } from "./api/client";
import {
  type AllowlistEntryView,
  blockedReason,
  canDemote,
  canRemove,
  entryLabel,
  entryPath,
  entryRef,
  isPartialRemoval,
  refPath,
  refProblem,
  refusalText,
  removalQuestion,
  sortEntries,
  unboundNote,
} from "./allowlist";

/**
 * The People section's logic, without a page.
 *
 * @decision(DL-129) @decision(DL-128)
 */

function entry(over: Partial<AllowlistEntryView>): AllowlistEntryView {
  return { kind: "email", email: "", admin: false, you: false, created_at: "2026-11-01T00:00:00Z", ...over };
}
const acct = (login: string, over: Partial<AllowlistEntryView> = {}) =>
  entry({ kind: "github", github_login: login, github_id: login.length, ...over });
const addr = (email: string, over: Partial<AllowlistEntryView> = {}) => entry({ email, ...over });

describe("entryRef", () => {
  it("reads an address when @ comes after the first character", () => {
    expect(entryRef(" Bob@Example.com ")).toEqual({ kind: "email", email: "bob@example.com" });
    expect(entryRef("a@b")).toEqual({ kind: "email", email: "a@b" });
  });
  it("reads a GitHub username with or without its @", () => {
    expect(entryRef("@octocat")).toEqual({ kind: "github", login: "octocat" });
    expect(entryRef("octocat")).toEqual({ kind: "github", login: "octocat" });
    expect(entryRef("  Octo-Cat ")).toEqual({ kind: "github", login: "Octo-Cat" });
  });
  it("is null for nothing", () => {
    expect(entryRef("   ")).toBeNull();
  });
  it("names the path segment the API is sent", () => {
    expect(refPath({ kind: "github", login: "bob" })).toBe("@bob");
    expect(refPath({ kind: "email", email: "bob@example.com" })).toBe("bob@example.com");
  });
});

describe("refProblem", () => {
  it("accepts GitHub's login shape and a plain address", () => {
    for (const t of ["octocat", "@a", "a-b-c", "x".repeat(39), "bob@example.com"]) {
      expect(refProblem(entryRef(t)), t).toBe("");
    }
  });
  it("refuses what cannot be a login or an address, before any request", () => {
    for (const t of ["-bob", "bob-", "bo--b", "bob.smith", "x".repeat(40), "../users", "bob/x", "@", "a@b@c", "a@b/c", "a b@c"]) {
      expect(refProblem(entryRef(t)), t).not.toBe("");
    }
    expect(refProblem(null)).toContain("GitHub username or an address");
  });
});

describe("naming an entry", () => {
  it("shows an account entry by its login and addresses it by its ID", () => {
    const octo = acct("Octo", { github_id: 583231 });
    expect(entryLabel(octo)).toBe("@Octo");
    expect(entryPath(octo)).toBe("id:583231");
    expect(entryLabel(addr("bob@example.com"))).toBe("bob@example.com");
    expect(entryPath(addr("bob@example.com"))).toBe("bob@example.com");
  });
  it("tells two entries with one stale login apart", () => {
    expect(entryPath(acct("carol", { github_id: 1 }))).not.toBe(entryPath(acct("carol", { github_id: 2 })));
  });
});

describe("sortEntries", () => {
  it("puts administrators first, then sorts by name, and leaves its input alone", () => {
    const list = [
      addr("zed@example.com"),
      acct("bob", { name: "Bob" }),
      addr("root@example.com", { admin: true, name: "Root" }),
      acct("ann", { admin: true }),
      acct("carl", { name: "alice" }),
    ];
    const before = list.map(entryLabel);
    expect(sortEntries(list).map(entryLabel)).toEqual([
      "@ann",
      "root@example.com",
      "@carl",
      "@bob",
      "zed@example.com",
    ]);
    expect(list.map(entryLabel)).toEqual(before);
  });
});

describe("the guards", () => {
  it("never offers to remove or demote the last admin entry", () => {
    const last = addr("root@example.com", { admin: true });
    const list = [last, addr("bob@example.com")];
    expect(canRemove(last, list)).toBe(false);
    expect(canDemote(last, list)).toBe(false);
    expect(blockedReason(last, list)).toContain("last administrator");
    expect(canRemove(list[1], list)).toBe(true);
  });
  it("never offers to take the caller's only admin entry", () => {
    const mine = addr("me@example.com", { admin: true, you: true });
    const other = acct("root", { admin: true });
    const list = [mine, other];
    expect(canRemove(mine, list)).toBe(false);
    expect(canDemote(mine, list)).toBe(false);
    expect(blockedReason(mine, list)).toContain("only administrator entry");
    expect(canRemove(other, list)).toBe(true);
    expect(canDemote(other, list)).toBe(true);
  });
  it("offers one of the caller's two admin entries", () => {
    const a = addr("me@example.com", { admin: true, you: true });
    const b = acct("me", { admin: true, you: true });
    const list = [a, b];
    expect(canRemove(a, list)).toBe(true);
    expect(canDemote(b, list)).toBe(true);
  });
  it("does not count a plain entry of the caller's as keeping them an admin", () => {
    const a = addr("me@example.com", { admin: true, you: true });
    const b = acct("me", { you: true });
    const list = [a, b, acct("root", { admin: true })];
    expect(canRemove(a, list)).toBe(false);
    expect(canRemove(b, list)).toBe(true);
  });
  it("has nothing to demote on a plain entry", () => {
    const plain = addr("bob@example.com");
    expect(canDemote(plain, [plain, addr("r@example.com", { admin: true })])).toBe(false);
    expect(blockedReason(plain, [plain])).toBe("");
  });
});

describe("removalQuestion", () => {
  it("says what removal ends for someone who signed in", () => {
    const q = removalQuestion(acct("bob", { last_login_at: "2026-11-02T00:00:00Z" }));
    for (const part of [
      "@bob",
      "browser sessions",
      "cancels their jobs",
      "stops their sessions",
      "revokes their CLI tokens",
      "deletes their stored credentials",
      "history and templates are kept",
    ]) {
      expect(q).toContain(part);
    }
  });
  it("says another entry keeps the person's access", () => {
    const q = removalQuestion(addr("bob@example.com", { last_login_at: "x", also_covered_by: ["@bob"] }));
    expect(q).toContain("keep their access through @bob");
    expect(q).toContain("nothing of theirs is ended");
    expect(q).not.toContain("revokes");
  });
  it("warns that an address entry over an unbound row ends everything, and says to sign in first", () => {
    const e = addr("alice@corp.example", { last_login_at: "x", unbound: true });
    const q = removalQuestion(e);
    expect(q).toContain("even if their GitHub account has an entry");
    expect(q).toContain("revokes their CLI tokens");
    expect(q).toContain("sign in once first");
    expect(unboundNote(e)).toContain("Has not signed in since accounts were added");
    expect(unboundNote(addr("b@example.com"))).toBe("");
  });
  it("says nothing else ends for an entry nobody used", () => {
    expect(removalQuestion(addr("new@example.com"))).toContain("Nobody has signed in");
  });
});

describe("a refusal", () => {
  it("is shown in the server's words", () => {
    const err = new ApiError(422, { type: "about:blank", title: "Unprocessable Entity", detail: "GitHub knows no account named @nobody", status: 422 });
    expect(refusalText(err)).toBe("GitHub knows no account named @nobody");
    expect(refusalText(new ApiError(502, { type: "about:blank", title: "Bad Gateway", status: 502 }))).toBe("Bad Gateway");
  });
  it("tells a removal that half finished from one that was refused", () => {
    expect(isPartialRemoval(new ApiError(500, { type: "about:blank", title: "Internal Server Error", status: 500 }))).toBe(true);
    expect(isPartialRemoval(new ApiError(422, { type: "about:blank", title: "Unprocessable Entity", status: 422 }))).toBe(false);
    expect(isPartialRemoval(new Error("offline"))).toBe(false);
  });
});
