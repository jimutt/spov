/**
 * A job's deliverables as the page shows them: the pull requests and
 * reports in its inventory, the URLs their files are read from, and how
 * a report's image links resolve.
 *
 * @decision(DL-051) @decision(DL-063)
 */
import type { components } from "./api/types";
import type { Job, Mark } from "./format";

export type Deliverable = components["schemas"]["DeliverableView"];

/**
 * fileURL is where a collected file is read: a report, or an image one
 * links. `download` asks for it as an attachment, which for a report with
 * images is a zip of it and them.
 *
 * @decision(DL-051)
 */
export function fileURL(jobID: string, path: string, download = false): string {
  const q = new URLSearchParams({ path });
  if (download) q.set("download", "true");
  return `/api/v1/jobs/${encodeURIComponent(jobID)}/file?${q.toString()}`;
}

/**
 * outRelative resolves a link destination in a report to a path under
 * the job's out directory, or undefined for a URL, an absolute path or
 * anything that leaves the directory. It mirrors `jobs.OutRelative`; the
 * server stored only what that accepted.
 *
 * @decision(DL-063)
 */
export function outRelative(dest: string): string | undefined {
  let d = dest.trim().replace(/^</, "").replace(/>$/, "");
  if (!d || d.includes(":") || d.startsWith("/") || d.startsWith("\\") || d.startsWith("#")) {
    return undefined;
  }
  d = d.replace(/[?#].*$/, "");
  try {
    d = decodeURIComponent(d);
  } catch {
    return undefined;
  }
  if (d.includes("\\") || d.includes("\0")) return undefined;
  const out: string[] = [];
  for (const seg of d.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (!out.length) return undefined;
      out.pop();
      continue;
    }
    out.push(seg);
  }
  return out.length ? out.join("/") : undefined;
}

/**
 * imageSource is the renderer's image resolver for one job's report: a
 * link inside the out directory becomes that job's file URL, absolute on
 * this origin, and anything else stays alt text. An image that was not
 * collected answers 404 from spov, never a request to the agent's URL.
 *
 * @decision(DL-063)
 */
export function imageSource(jobID: string, origin: string): { prefix: string; resolve: (dest: string) => string | undefined } {
  const prefix = `${origin}/api/v1/jobs/${encodeURIComponent(jobID)}/file?`;
  return {
    prefix,
    resolve: (dest) => {
      const p = outRelative(dest);
      return p ? origin + fileURL(jobID, p) : undefined;
    },
  };
}

/**
 * missingImage stands in for a report's image that did not load: an image
 * spov could not collect answers 404. The image becomes its alt text and a
 * note, as plain text, so the preview never draws a broken picture.
 * Given the capture-phase error event of the preview's container; any
 * other target is left alone.
 *
 * @decision(DL-063) @decision(DL-051)
 */
export function missingImage(event: Event): void {
  const img = event.target;
  if (!(img instanceof HTMLImageElement) || !img.parentNode) return;
  const note = img.ownerDocument.createElement("span");
  note.className = "text-soft";
  const alt = img.alt.trim();
  note.textContent = alt ? `[${alt}: an image that was not collected]` : "[an image that was not collected]";
  img.replaceWith(note);
}

/** pullRequests and reports split an inventory by kind. */
export function pullRequests(job: Job | undefined): Deliverable[] {
  return (job?.deliverables ?? []).filter((d) => d.kind === "pr");
}
export function reports(job: Job | undefined): Deliverable[] {
  return (job?.deliverables ?? []).filter((d) => d.kind === "report");
}
/** syncState is a sync job's one member, its measured repository. @decision(DL-101) */
export function syncState(job: Job | undefined): Deliverable | undefined {
  return (job?.deliverables ?? []).find((d) => d.kind === "sync");
}

/**
 * changeNoun is what a forge calls a change request: a merge request on
 * GitLab, a pull request elsewhere. It mirrors git.ChangeNoun.
 *
 * @decision(DL-051)
 */
export function changeNoun(forge: string | undefined): string {
  return forge === "gitlab" ? "merge request" : "pull request";
}

/**
 * changeRef is a change request's number as its forge writes it: !12 on
 * GitLab, #12 elsewhere. It mirrors git.ChangeRef.
 *
 * @decision(DL-051)
 */
export function changeRef(forge: string | undefined, n: number): string {
  return `${forge === "gitlab" ? "!" : "#"}${n}`;
}

/**
 * changeNounOf names a job's change requests: merge requests when every
 * one is GitLab's, pull requests otherwise.
 *
 * @decision(DL-051)
 */
export function changeNounOf(members: Deliverable[]): string {
  const nouns = new Set(members.filter((d) => d.kind === "pr").map((d) => changeNoun(d.forge)));
  return nouns.size === 1 ? [...nouns][0] : "pull request";
}

/**
 * pullRequestName names a pull request, or a merge request on GitLab: by
 * number once the forge answered (#12, or !12 on GitLab), else by its
 * branch.
 *
 * @decision(DL-051)
 */
export function pullRequestName(d: Deliverable): string {
  const noun = changeNoun(d.forge);
  const title = noun[0].toUpperCase() + noun.slice(1);
  if (!d.number) return `${title} from ${d.branch ?? "a branch"}`;
  return `${title} ${changeRef(d.forge, d.number)}`;
}

/** STATUS_WORDS says what was found about one deliverable. @decision(DL-051) */
export const STATUS_WORDS: Record<Deliverable["status"], string> = {
  verified: "Verified",
  missing: "Not found",
  unverified: "Not checked",
  over_limit: "Too large",
  // @decision(DL-051)
  withdrawn: "Withdrawn",
};

/** sizeText is a file's size for a person. */
export function sizeText(bytes: number | undefined): string {
  const n = bytes ?? 0;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KB`;
  return `${n} bytes`;
}

/**
 * deliveredSummary counts what a finished job delivered, for a listing
 * row: "2 pull requests", "1 report". Only verified members count.
 *
 * @decision(DL-051) @decision(DL-060)
 */
export function deliveredSummary(job: Job): string {
  const verified = (job.deliverables ?? []).filter((d) => d.status === "verified");
  if (!verified.length) return "";
  // @decision(DL-101)
  if (job.kind === "sync") return job.partial ? "Repository changes, partly done" : "Repository changes";
  // @decision(DL-051)
  const noun = job.kind === "pr" ? changeNounOf(verified) : "report";
  const text = `${verified.length} ${noun}${verified.length === 1 ? "" : "s"}`;
  return job.partial ? `${text}, partly done` : text;
}

/**
 * deliverableMark draws a status: a tick for verified, a diamond for one
 * the forge said is not there, and a neutral ring for one spov could not
 * check or did not keep.
 *
 * @decision(DL-051) @decision(DL-056)
 */
export function deliverableMark(status: Deliverable["status"]): Mark {
  switch (status) {
    case "verified":
      return "done";
    case "missing":
      return "fail";
    default:
      return "stop";
  }
}
