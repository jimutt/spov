import type { components } from "./api/types";

/**
 * The Organisation section's logic: the shared GitHub App's status in
 * words, and the body an admin stores. Admin only.
 *
 * @decision(DL-062) @decision(DL-042)
 */

export type OrgGitHubView = components["schemas"]["OrgGitHubView"];
export type AdminEventView = components["schemas"]["AdminEventView"];

/** sourceLine says where a platform's App comes from. */
export function sourceLine(v: OrgGitHubView): string {
  switch (v.source) {
    case "environment":
      return "Set by the deployment's environment, so it is changed there, not here.";
    case "stored":
      return `Stored here${v.updated_by ? ` by ${v.updated_by}` : ""}.`;
    default:
      return "No App is configured, so only people's own credentials reach this platform.";
  }
}

/**
 * checkLine says what the latest check found: where the App is
 * installed and how much it reaches, or why it failed.
 *
 * @decision(DL-042)
 */
export function checkLine(v: OrgGitHubView): { ok: boolean; text: string } | null {
  const c = v.check;
  if (!c) return null;
  if (c.error) return { ok: false, text: `The check failed: ${c.error}` };
  const n = c.repo_count;
  const where = c.account ? `Installed on ${c.account}` : "Installed";
  return { ok: true, text: `${where}, reaching ${n} ${n === 1 ? "repository" : "repositories"}.` };
}

/**
 * missingLine names the permissions sandboxes ask for that the
 * installation lacks, which their git and pull requests would miss.
 *
 * @decision(DL-042) @decision(DL-034)
 */
export function missingLine(v: OrgGitHubView): string {
  const missing = v.check?.missing ?? [];
  if (missing.length === 0) return "";
  return `It lacks ${missing.map((m) => m.replace(":", " ")).join(", ")}, which sandboxes ask for.`;
}

/** The store form's state. */
export interface AppDraft {
  appID: string;
  account: string;
  key: string;
}

/** draftProblems says what stops a store, before a request is made. */
export function appProblems(d: AppDraft): string[] {
  const out: string[] = [];
  if (!d.appID.trim()) out.push("the App's ID is needed");
  if (!d.key.trim()) out.push("the App's private key is needed");
  else if (!d.key.includes("PRIVATE KEY")) out.push("the private key is the PEM file GitHub gave you");
  return out;
}

/** toAppBody is what PUT /api/v1/org/github/{platform} is sent. */
export function toAppBody(d: AppDraft): { app_id: string; private_key: string; account?: string } {
  const body: { app_id: string; private_key: string; account?: string } = { app_id: d.appID.trim(), private_key: d.key.trim() };
  if (d.account.trim()) body.account = d.account.trim();
  return body;
}

/** eventLine says one audited change in words. */
export function eventLine(e: AdminEventView): string {
  const what =
    e.action === "github_app.stored" ? "stored" : e.action === "github_app.removed" ? "removed" : e.action;
  return `${e.actor} ${what} ${e.detail ?? ""}`.trim();
}
