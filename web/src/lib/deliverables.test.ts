import { describe, expect, it } from "vitest";
import { changeNounOf, deliveredSummary, fileURL, missingImage, outRelative, pullRequestName, syncState, type Deliverable } from "./deliverables";
import type { Job } from "./format";

describe("where a job's files are read", () => {
  // @decision(DL-051)
  it("asks the file endpoint by path, and for a download when asked", () => {
    expect(fileURL("j 1", "images/a b.png")).toBe("/api/v1/jobs/j%201/file?path=images%2Fa+b.png");
    expect(fileURL("j1", "a.md", true)).toBe("/api/v1/jobs/j1/file?path=a.md&download=true");
  });

  // @decision(DL-063)
  it("resolves a link the way the server does, or not at all", () => {
    const cases: Record<string, string | undefined> = {
      "images/a.png": "images/a.png",
      "./images/a.png": "images/a.png",
      "images/../a.png": "a.png",
      "<images/my file.png>": "images/my file.png",
      "images/my%20file.png": "images/my file.png",
      "a.png?x=1": "a.png",
      "../a.png": undefined,
      "images/../../a.png": undefined,
      "/etc/passwd": undefined,
      "https://example.com": undefined,
      "data:image/png;base64": undefined,
      "javascript:alert(1)": undefined,
      "#heading": undefined,
      "..": undefined,
      "a\\b.png": undefined,
      "%2e%2e/a.png": undefined,
    };
    for (const [dest, want] of Object.entries(cases)) {
      expect(outRelative(dest), dest).toBe(want);
    }
  });

  // @decision(DL-051)
  it("counts only what was verified", () => {
    const job = {
      kind: "report",
      deliverables: [
        { kind: "report", status: "verified", path: "a.md" },
        { kind: "report", status: "over_limit", path: "b.md" },
      ],
    } as Job;
    expect(deliveredSummary(job)).toBe("1 report");
    expect(deliveredSummary({ ...job, partial: true })).toBe("1 report, partly done");
    expect(deliveredSummary({ ...job, deliverables: [] })).toBe("");
  });
});

describe("a change request's name", () => {
  const pr = (forge: string, number?: number): Deliverable =>
    ({ kind: "pr", status: "verified", forge, number, branch: "spov/job" }) as Deliverable;
  // @decision(DL-051)
  it("is a merge request on GitLab and a pull request elsewhere", () => {
    expect(pullRequestName(pr("gitlab", 4))).toBe("Merge request !4");
    expect(pullRequestName(pr("gitlab"))).toBe("Merge request from spov/job");
    expect(pullRequestName(pr("github", 4))).toBe("Pull request #4");
    expect(pullRequestName(pr("", 4))).toBe("Pull request #4");
    expect(changeNounOf([pr("gitlab"), pr("gitlab")])).toBe("merge request");
    expect(changeNounOf([pr("gitlab"), pr("forgejo")])).toBe("pull request");
    expect(deliveredSummary({ kind: "pr", deliverables: [pr("gitlab", 1), pr("gitlab", 2)] } as Job)).toBe(
      "2 merge requests",
    );
  });
});

describe("an image in a report that did not load", () => {
  // @decision(DL-051) @decision(DL-063)
  it("becomes its alt text as plain text, and nothing else is touched", () => {
    const box = document.createElement("div");
    box.innerHTML = '<p>See <img alt="the <b>diagram</b>" src="x"> and <a href="y">this</a></p>';
    const img = box.querySelector("img")!;
    missingImage({ target: img } as unknown as Event);
    expect(box.querySelector("img")).toBeNull();
    expect(box.querySelector("b")).toBeNull();
    expect(box.textContent).toBe("See [the <b>diagram</b>: an image that was not collected] and this");

    missingImage({ target: box.querySelector("a") } as unknown as Event);
    expect(box.querySelector("a")).not.toBeNull();
  });
});

describe("a sync job's deliverable", () => {
  // @decision(DL-101)
  it("is its repository, summed up as changes rather than reports", () => {
    const member = { kind: "sync", status: "verified", repository: "acme/app", branch: "main" } as Deliverable;
    const job = { kind: "sync", deliverables: [member] } as unknown as Job;
    expect(syncState(job)).toBe(member);
    expect(deliveredSummary(job)).toBe("Repository changes");
    expect(syncState({ kind: "report", deliverables: [] } as unknown as Job)).toBeUndefined();
  });
});
