import { describe, expect, it } from "vitest";
import { ApiError } from "./api/client";
import {
  type ServiceAccountView,
  type ServiceTokenView,
  accountGone,
  accountPath,
  authorPatch,
  copyText,
  createRequest,
  dayText,
  deletedText,
  deletionQuestion,
  expiryText,
  isPartialDeletion,
  issuerText,
  labelProblem,
  lastUsedText,
  lifetimeProblem,
  nameProblem,
  partialDeletion,
  sortAccounts,
  tokenCountText,
  tokenPath,
  tokenRequest,
} from "./serviceaccounts";

/**
 * The Service accounts section's logic, without a page.
 *
 * @decision(DL-142)
 */

function account(name: string, over: Partial<ServiceAccountView> = {}): ServiceAccountView {
  return {
    id: "01JC0000000000000000000000",
    name,
    git_name: name,
    git_email: `${name}@service.invalid`,
    created_at: "2026-11-01T00:00:00Z",
    tokens: 0,
    ...over,
  };
}

function token(over: Partial<ServiceTokenView> = {}): ServiceTokenView {
  return { id: "01JC0000000000000000000001", name: "ci", created_at: "2026-11-01T00:00:00Z", ...over };
}

const now = new Date(2026, 10, 17, 12, 0, 0);

describe("nameProblem", () => {
  it("accepts the server's shape", () => {
    for (const t of ["abc", "release-bot", "a1-b2", "a" + "b".repeat(30) + "c", " ci-bot "]) {
      expect(nameProblem(t), t).toBe("");
    }
  });
  it("refuses anything else, before any request", () => {
    for (const t of ["ab", "1bot", "bot-", "Bot", "bot_x", "a".repeat(33), "../users", "bot/tokens", "bot%2F", "b ot", ".."]) {
      expect(nameProblem(t), t).not.toBe("");
    }
    expect(nameProblem("  ")).toContain("Name the account");
  });
});

describe("the path segments", () => {
  it("sends only a name of the server's shape", () => {
    expect(accountPath("release-bot")).toBe("release-bot");
    for (const t of ["..", "a/b", "release-bot/tokens", "", " release-bot"]) expect(accountPath(t), t).toBeNull();
  });
  it("sends only a ULID as a token ID", () => {
    expect(tokenPath("01JCZ8Y3N5Q2W9V7T6R4P3M2K1")).toBe("01JCZ8Y3N5Q2W9V7T6R4P3M2K1");
    for (const t of ["..", ".", "", "01JCZ8Y3N5Q2W9V7T6R4P3M2K", "01JCZ8Y3N5Q2W9V7T6R4P3M2KI", "../../x", "id/x"]) {
      expect(tokenPath(t), t).toBeNull();
    }
  });
});

describe("sortAccounts", () => {
  it("puts deleted accounts last, each group by name, and leaves its input alone", () => {
    const list = [
      account("zeta"),
      account("old-bot", { disabled_at: "2026-11-02T00:00:00Z" }),
      account("alpha"),
      account("aaa-gone", { disabled_at: "2026-11-03T00:00:00Z" }),
    ];
    const before = list.map((a) => a.name);
    expect(sortAccounts(list).map((a) => a.name)).toEqual(["alpha", "zeta", "aaa-gone", "old-bot"]);
    expect(list.map((a) => a.name)).toEqual(before);
  });
});

describe("the requests", () => {
  it("leaves empty author fields out of a create", () => {
    expect(createRequest(" ci-bot ", "  ", "")).toEqual({ name: "ci-bot" });
    expect(createRequest("ci-bot", " CI ", "ci@example.com")).toEqual({
      name: "ci-bot",
      git_name: "CI",
      git_email: "ci@example.com",
    });
  });
  it("patches only the author fields that changed", () => {
    const a = account("ci-bot", { git_name: "CI", git_email: "ci@example.com" });
    expect(authorPatch(a, "CI", "ci@example.com")).toBeNull();
    expect(authorPatch(a, " CI ", "ci@example.com")).toBeNull();
    expect(authorPatch(a, "Releases", "ci@example.com")).toEqual({ git_name: "Releases" });
    expect(authorPatch(a, "CI", "rel@example.com")).toEqual({ git_email: "rel@example.com" });
    // An emptied field is sent, and the server says why it is refused.
    expect(authorPatch(a, "", "ci@example.com")).toEqual({ git_name: "" });
  });
  it("leaves the default lifetime out of a token request", () => {
    expect(tokenRequest(" ci ", "default", "")).toEqual({ name: "ci" });
    expect(tokenRequest("ci", "days", " 30 ")).toEqual({ name: "ci", expires_in_days: 30 });
    expect(tokenRequest("ci", "never", "30")).toEqual({ name: "ci", no_expiry: true });
  });
});

describe("the token form's checks", () => {
  it("needs a label of at most 64 characters", () => {
    expect(labelProblem("release workflow")).toBe("");
    expect(labelProblem("é".repeat(64))).toBe("");
    expect(labelProblem("x".repeat(65))).toContain("at most 64");
    expect(labelProblem("   ")).toContain("Name the token");
  });
  it("needs a lifetime of 1 to 1095 days when it names one", () => {
    for (const d of ["1", "30", "1095", " 365 "]) expect(lifetimeProblem("days", d), d).toBe("");
    for (const d of ["", "0", "1096", "-1", "1.5", "1e2", "ten"]) expect(lifetimeProblem("days", d), d).toContain("1 to 1095");
    expect(lifetimeProblem("default", "")).toBe("");
    expect(lifetimeProblem("never", "0")).toBe("");
  });
});

describe("what a row says", () => {
  it("names a day relative to today, with the year when it is another", () => {
    expect(dayText(new Date(2026, 10, 17, 8).toISOString(), now)).toBe("today");
    expect(dayText(new Date(2026, 10, 16, 8).toISOString(), now)).toBe("yesterday");
    expect(dayText(new Date(2026, 10, 18, 8).toISOString(), now)).toBe("tomorrow");
    expect(dayText(new Date(2027, 10, 17, 8).toISOString(), now)).toContain("2027");
    expect(dayText(new Date(2026, 2, 3, 8).toISOString(), now)).not.toContain("2026");
    expect(dayText("not a date", now)).toBe("");
  });
  it("says when an account or token was last used, or never", () => {
    expect(lastUsedText(undefined, now)).toBe("Never used");
    expect(lastUsedText(new Date(2026, 10, 17, 8).toISOString(), now)).toBe("Last used today");
  });
  it("counts usable tokens", () => {
    expect(tokenCountText(0)).toBe("No usable tokens");
    expect(tokenCountText(1)).toBe("1 usable token");
    expect(tokenCountText(3)).toBe("3 usable tokens");
  });
  it("marks a deleted account with its date", () => {
    expect(deletedText(account("gone", { disabled_at: new Date(2026, 10, 16).toISOString() }), now)).toBe("Deleted yesterday");
    expect(deletedText(account("here"), now)).toBe("");
  });
  it("says when a token expires, has expired, or never does", () => {
    expect(expiryText(token(), now)).toBe("Never expires");
    expect(expiryText(token({ expires_at: new Date(2027, 10, 17).toISOString() }), now)).toMatch(/^Expires .*2027/);
    expect(expiryText(token({ expires_at: new Date(2026, 10, 16).toISOString() }), now)).toBe("Expired yesterday");
  });
  it("names the issuer, or says it is unknown", () => {
    expect(issuerText(token({ issued_by: { id: "u", label: "@root" } }))).toBe("@root");
    expect(issuerText(token({ issued_by: { id: "u" } }))).toBe("unknown");
    expect(issuerText(token())).toBe("unknown");
  });
});

describe("deleting", () => {
  it("says what the delete ends and keeps", () => {
    const q = deletionQuestion(account("release-bot"));
    for (const part of [
      "release-bot",
      "cancels its jobs",
      "stops its sessions",
      "revokes its tokens",
      "deletes its stored credentials",
      "history is kept",
      "cannot be used again",
    ]) {
      expect(q).toContain(part);
    }
  });
  it("tells a delete that half finished from one that was refused", () => {
    expect(isPartialDeletion(new ApiError(500, { type: "about:blank", title: "Internal Server Error", status: 500 }))).toBe(true);
    expect(isPartialDeletion(new ApiError(404, { type: "about:blank", title: "Not Found", status: 404 }))).toBe(false);
    expect(isPartialDeletion(new Error("offline"))).toBe(false);
  });
});

describe("copyText", () => {
  it("reports whether the clipboard took the text", async () => {
    const took: string[] = [];
    expect(await copyText("t", { writeText: async (s) => void took.push(s) })).toBe(true);
    expect(took).toEqual(["t"]);
    expect(await copyText("t", { writeText: async () => Promise.reject(new Error("denied")) })).toBe(false);
    expect(await copyText("t", undefined)).toBe(false);
  });
});

describe("reading the list again", () => {
  const list = [account("live"), account("gone", { disabled_at: "2026-11-17T00:00:00Z" })];
  it("tells a live account from a deleted or missing one", () => {
    expect(accountGone(list, "live")).toBe(false);
    expect(accountGone(list, "gone")).toBe(true);
    expect(accountGone(list, "never-was")).toBe(true);
    expect(accountGone(undefined, "live")).toBe(true);
  });
  it("calls a 500 partial only when the account is deleted afterwards", () => {
    const err = new ApiError(500, { type: "about:blank", title: "Internal Server Error", status: 500 });
    expect(partialDeletion(err, list, "gone")).toBe(true);
    expect(partialDeletion(err, list, "live")).toBe(false);
    expect(partialDeletion(err, list, "never-was")).toBe(false);
    expect(partialDeletion(new ApiError(409, { type: "about:blank", title: "Conflict", status: 409 }), list, "gone")).toBe(false);
  });
});
