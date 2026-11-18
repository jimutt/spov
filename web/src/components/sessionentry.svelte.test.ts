import { afterEach, describe, expect, it } from "vitest";
import { flushSync, mount, unmount } from "svelte";
import SessionEntry from "./SessionEntry.svelte";
import type { Session } from "../lib/format";

/**
 * The list row mounted for its markup: what the row's link says and
 * what the meta line carries. The same markup serves every width.
 *
 * @decision(DL-147)
 */

const base = {
  id: "01SESSION",
  name: "job-01m7nwex",
  state: "running",
  created_at: "2026-11-18T10:00:00Z",
} as unknown as Session;

let app: ReturnType<typeof mount> | undefined;

function draw(session: Session) {
  document.body.innerHTML = "";
  app = mount(SessionEntry, { target: document.body, props: { session, now: new Date("2026-11-18T10:05:00Z") } });
  flushSync();
  const link = document.querySelector("a")!;
  const meta = link.nextElementSibling!;
  return { link, meta: meta.textContent!.replace(/\s+/g, " ").trim() };
}

afterEach(() => {
  if (app) unmount(app);
  app = undefined;
});

describe("a session list row", () => {
  it("leads with the repository and keeps the name on the meta line", () => {
    const { link, meta } = draw({ ...base, repos: [{ owner: "acme", name: "app" }] } as Session);
    expect(link.textContent!.trim()).toBe("acme/app");
    expect(link.getAttribute("href")).toBe("/session/?id=01SESSION");
    expect(link.hasAttribute("title")).toBe(false);
    expect(meta).toBe("Interactive job-01m7nwex");
  });

  it("counts further repositories and names them all in the title", () => {
    const { link, meta } = draw({
      ...base,
      repos: [
        { owner: "acme", name: "app" },
        { owner: "acme", name: "lib" },
      ],
    } as Session);
    expect(link.textContent!.replace(/\s+/g, "")).toBe("acme/app+1");
    expect(link.getAttribute("title")).toBe("acme/app, acme/lib");
    expect(meta).not.toContain("acme/lib");
  });

  // @decision(DL-151)
  it("leads with the title, and carries the repository and the name on the meta line", () => {
    const title = "Fix the flaky login test in the sign-in flow";
    const { link, meta } = draw({
      ...base,
      title,
      job: { id: "01JOB", kind: "report", state: "running" },
      repos: [
        { owner: "acme", name: "app" },
        { owner: "acme", name: "lib" },
      ],
    } as Session);
    expect(link.textContent!.trim()).toBe(title);
    expect(link.getAttribute("title")).toBe(title);
    expect(link.className).toContain("truncate");
    expect(link.getAttribute("href")).toBe("/session/?id=01SESSION");
    expect(meta).toBe("Report job acme/app +1 job-01m7nwex");
    const repo = Array.from(link.nextElementSibling!.querySelectorAll("span")).find((e) => e.textContent === "acme/app +1")!;
    expect(repo.className).toContain("min-w-0");
    expect(repo.className).toContain("truncate");
    expect(document.querySelectorAll("a")).toHaveLength(1);
  });

  // @decision(DL-151)
  it("draws a title as text", () => {
    const { link } = draw({ ...base, title: "<b>bold</b> & <img src=x>", repos: [] } as Session);
    expect(link.textContent!.trim()).toBe("<b>bold</b> & <img src=x>");
    expect(link.querySelector("b, img")).toBeNull();
  });

  it("leads with the name when the session has no repository", () => {
    const { link, meta } = draw({ ...base, repos: [] } as Session);
    expect(link.textContent!.trim()).toBe("job-01m7nwex");
    expect(meta).toBe("Interactive");
  });
});
