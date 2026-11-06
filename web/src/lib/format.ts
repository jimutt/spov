import type { components } from "./api/types";

export type Session = components["schemas"]["SessionView"];
export type SandboxConfig = components["schemas"]["SandboxConfigView"];

/** The four tones the palette carries for a session's state. */
export type Tone = "run" | "start" | "fail" | "stop";

/**
 * tone maps a session state onto the palette. An unknown state — a
 * newer server — reads as stopped rather than throwing: the list must
 * still render.
 */
export function tone(state: string): Tone {
  switch (state) {
    case "running":
      return "run";
    case "starting":
      return "start";
    case "failed":
      return "fail";
    default:
      return "stop";
  }
}

/**
 * duration renders a span the way the reference does: `1h 12m`, with the
 * minutes zero-padded so a column of them lines up. Days appear only
 * once there are any.
 */
export function duration(secs: number): string {
  const s = Math.max(0, Math.floor(secs));
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const mins = Math.floor((s % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  return `${hours}h ${String(mins).padStart(2, "0")}m`;
}

/**
 * age is how long a session has run: to now while it is live, and to the
 * moment it stopped once it is not — so a finished session shows how
 * long it took rather than how long ago it was.
 */
export function age(s: Session, now: Date): number {
  const end = s.stopped_at ? Date.parse(s.stopped_at) : now.getTime();
  const start = Date.parse(s.created_at);
  if (Number.isNaN(start) || Number.isNaN(end)) return 0;
  return Math.max(0, Math.floor((end - start) / 1000));
}

/** live reports whether a session is still costing money. */
export function live(s: Session): boolean {
  return s.state === "running" || s.state === "starting";
}

/**
 * WarnWithin is how close to its hard timeout a running session has to
 * be before the page raises its voice — the hard timeout rather than
 * the idle one, which the API cannot report a countdown for.
 *
 * @decision(DL-055)
 */
export const WarnWithin = 15 * 60;

export interface Lifetime {
  /** 0..1 of the configured timeout that has been used. */
  fraction: number;
  /** The caption under the track. */
  note: string;
  /** Whether the caption is the one warning the page raises. */
  warn: boolean;
}

/**
 * lifetime places a session against its configured timeout: how much is
 * used, and what to say about it. A session with no timeout configured
 * has no bound to draw, so the track is empty.
 *
 * @decision(DL-055)
 */
export function lifetime(s: Session, now: Date): Lifetime {
  const cap = s.sandbox?.timeout_secs ?? 0;
  const used = age(s, now);
  if (cap <= 0) {
    return { fraction: 0, note: live(s) ? "No limit set" : "Ended", warn: false };
  }
  const fraction = Math.min(1, used / cap);
  if (!live(s)) {
    // A session that ran to its cap was stopped by it; one that did not
    // ended for some other reason, which the state already names.
    return {
      fraction,
      note: used >= cap ? "Hit its limit" : "Ended early",
      warn: false,
    };
  }
  const left = cap - used;
  // Sentence case except "of 24h", which reads on from the duration
  // above it. @decision(DL-060)
  if (left <= WarnWithin) {
    return { fraction, note: `Stops in ${limitLength(Math.max(0, left) / 60)}`, warn: true };
  }
  return { fraction, note: `of ${limitLength(cap / 60)}`, warn: false };
}

/**
 * money renders a figure as the floor it is. The `≥` is the caller's:
 * every place this appears also carries the words, because a symbol on
 * its own is not the disclosure DL-009 asks for.
 */
export function money(n: number): string {
  return `$${(Number.isFinite(n) ? n : 0).toFixed(2)}`;
}

/** clock is the time of day an entry is filed under. */
export function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

/**
 * day is the entry's date, as a reader would say it: today and yesterday
 * by name, anything older by date.
 */
export function day(iso: string, now: Date): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const days = midnights(d, now);
  if (days === 0) return "today";
  if (days === 1) return "yesterday";
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

/** midnights counts calendar days between d and now, in local time. */
function midnights(d: Date, now: Date): number {
  const a = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const b = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((b.getTime() - a.getTime()) / 86_400_000);
}

/** resources is the one-line summary of what a session asked for. */
export function resources(sb: SandboxConfig | undefined): string {
  if (!sb) return "";
  const parts: string[] = [];
  if (sb.cpu) parts.push(`${sb.cpu} cpu`);
  if (sb.memory_mib) parts.push(`${sb.memory_mib} MiB`);
  return parts.join(", ");
}

/** started is a session's creation time in millis, 0 when unparseable —
 *  never NaN, which would make a comparator return NaN and leave the
 *  sort order undefined rather than merely wrong. */
function started(s: Session): number {
  const t = Date.parse(s.created_at);
  return Number.isNaN(t) ? 0 : t;
}

/**
 * byLiveThenRecent orders the list the way it is read: whatever is
 * running now first, then newest. The server returns `order by id`
 * (oldest ULID first), so sorting here keeps `spov ls`'s order alone.
 *
 * @decision(DL-060)
 */
export function byLiveThenRecent(a: Session, b: Session): number {
  const [la, lb] = [live(a), live(b)];
  if (la !== lb) return la ? -1 : 1;
  return started(b) - started(a);
}

/** repoNames is the session's repositories as `owner/name`. */
export function repoNames(s: Session): string[] {
  return (s.repos ?? []).map((r) => `${r.owner}/${r.name}`);
}

/**
 * speaker is what a transcript calls whoever said this. Lives here so
 * the list and the page cannot disagree on it.
 *
 * @decision(DL-058)
 */
export function speaker(role: string | undefined): string {
  if (role === "user") return "You";
  if (role === "assistant" || role === "agent") return "Agent";
  // spov's own continuation, not a party to the conversation.
  // @decision(DL-058)
  if (role === "synthetic") return "spov";
  return role || "Agent";
}

export type Job = components["schemas"]["JobView"];
export type Gate = components["schemas"]["GateResult"];

/**
 * jobTone maps a job state onto the same four tones a session uses.
 * `blocked` and `steered` read as `start`, not `fail`: both are
 * suspensions waiting on a person, not a failed run. An unknown state
 * reads as stopped rather than throwing, exactly as tone() does.
 *
 * @decision(DL-057) @decision(DL-045)
 */
export function jobTone(state: string): Tone {
  switch (state) {
    // A succeeded job takes the running colour, not the cancelled one.
    case "succeeded":
    case "running":
    case "delivering":
      return "run";
    case "queued":
    case "provisioning":
    case "blocked":
    case "steered":
      return "start";
    case "failed":
      return "fail";
    default:
      return "stop";
  }
}

/**
 * gateTone colours one gate's finding. `unverified` is deliberately not
 * a failure, and `partial` is a pass that something qualifies.
 *
 * @decision(DL-057) @decision(DL-050)
 */
export function gateTone(outcome: string): Tone {
  switch (outcome) {
    case "passed":
      return "run";
    case "partial":
      return "start";
    case "failed":
      return "fail";
    default:
      return "stop";
  }
}

/**
 * The marks a state is drawn with: a dot for running, a ring for
 * waiting, a tick for done, a gold tick for done in part, a diamond for
 * failed and a grey ring for stopped. StateMark.svelte draws them.
 *
 * @decision(DL-056) @decision(DL-060)
 */
export type Mark = "run" | "wait" | "done" | "part" | "fail" | "stop";

const markOfTone: Record<Tone, Mark> = { run: "run", start: "wait", fail: "fail", stop: "stop" };

/** sessionMark is a session state's mark, in tone()'s families. @decision(DL-056) */
export function sessionMark(state: string): Mark {
  return markOfTone[tone(state)];
}

/**
 * jobMark is a job state's mark: jobTone's family, with a tick for the
 * one state that is finished and good.
 *
 * @decision(DL-056)
 */
export function jobMark(state: string, partial = false): Mark {
  if (state === "succeeded") return partial ? "part" : "done";
  return markOfTone[jobTone(state)];
}

/**
 * jobWord is how a job's state reads beside its mark. A partial success
 * is still `succeeded`, said with "partly", as the CLI says it.
 *
 * @decision(DL-060) @decision(DL-050)
 */
export function jobWord(state: string, partial = false): string {
  return state === "succeeded" && partial ? "Succeeded, partly" : stateWord(state);
}

/** gateMark is a gate outcome's mark. `unverified` stays neutral. @decision(DL-056) */
export function gateMark(outcome: string): Mark {
  return outcome === "passed" ? "done" : markOfTone[gateTone(outcome)];
}

/** sentence capitalises the first letter, and leaves the rest alone. */
export function sentence(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

/**
 * stateWord is how a state reads beside its mark: "Steered", "Hit
 * limit". The API's value never changes; this is display only.
 *
 * @decision(DL-056)
 */
export function stateWord(state: string): string {
  return sentence(state.replace(/[_-]+/g, " "));
}

/**
 * KIND_NAMES is what a job kind is called on the page. A Record over the
 * API's kinds, so a new kind fails `astro check` until it is named.
 *
 * @decision(DL-056)
 */
export const KIND_NAMES: Record<Job["kind"], string> = {
  pr: "Pull request",
  report: "Report",
};

/** kindName names a kind, falling back to the API's word. @decision(DL-056) */
export function kindName(kind: string): string {
  return Object.hasOwn(KIND_NAMES, kind) ? KIND_NAMES[kind as Job["kind"]] : sentence(kind);
}

/** GATE_NAMES is what each of DL-050's gates is called. @decision(DL-056) */
export const GATE_NAMES: Record<string, string> = {
  check: "Check",
  deliverable: "Deliverable",
  conduct: "Conduct",
};

/** gateName names a gate, falling back to the verdict's own word. */
export function gateName(gate: string): string {
  return Object.hasOwn(GATE_NAMES, gate) ? GATE_NAMES[gate] : sentence(gate);
}

/** when is a start time as the list shows it: "Today 14:02". */
export function when(iso: string, now: Date): string {
  const d = day(iso, now);
  return d ? `${sentence(d)} ${clock(iso)}` : "";
}

/** What a session's State column says. @decision(DL-060) */
export interface ListState {
  mark: Mark;
  word: string;
  /** A short reason under the state, or "" when it needs none. */
  why: string;
}

/**
 * listState is the session list's State column. For a job's session it
 * is the JOB's state, which is what a reader scanning the list is
 * looking for, with a reason beneath when it matters. For an
 * interactive session it is the session's own state.
 *
 * @decision(DL-060)
 */
export function listState(s: Session): ListState {
  const j = s.job;
  if (!j) return { mark: sessionMark(s.state), word: stateWord(s.state), why: "" };
  let why = "";
  if (!live(s)) why = `Sandbox ${s.state}`;
  else if (j.state === "steered" || j.state === "blocked") why = "Waiting for you";
  return { mark: jobMark(j.state, j.partial), word: jobWord(j.state, j.partial), why };
}

/** The clock bar under a session page's title. @decision(DL-057) */
export interface ClockBar {
  /** 0..1 of the limit that is used. */
  fraction: number;
  /** The left caption: "Started 14:02". */
  start: string;
  /** The right caption's words, before its value: "Job stops at". */
  endLabel: string;
  /** The right caption's value, set in bold: "16:40". */
  endValue: string;
  /** Now, while the clock runs. Absent when it is paused. */
  now?: string;
  /** Whether the limit is within WarnWithin. */
  warn: boolean;
  /** The whole bar in words, for a screen reader. */
  label: string;
}

/**
 * clockBar is time against its limit: a running job's wall clock
 * (DL-049), else a live sandbox's timeout. Null for a session that has
 * ended or has no limit. A job's fraction counts time used before a
 * resume, the same arithmetic as wallClock's endsAt.
 *
 * @decision(DL-057) @decision(DL-049)
 */
export function clockBar(s: Session, j: Job | undefined, now: Date): ClockBar | null {
  const job = jobBar(j, now);
  if (job) return job;
  const cap = s.sandbox?.timeout_secs ?? 0;
  const created = Date.parse(s.created_at);
  if (!live(s) || cap <= 0 || Number.isNaN(created)) return null;
  const used = age(s, now);
  const start = `Started ${clock(s.created_at)}`;
  const endValue = clock(new Date(created + cap * 1000).toISOString());
  const at = clock(now.toISOString());
  return {
    fraction: Math.min(1, used / cap),
    start,
    endLabel: "Sandbox stops at",
    endValue,
    now: at,
    warn: cap - used <= WarnWithin,
    label: `Sandbox time: ${start}, now ${at}, stops at ${endValue}`,
  };
}

function jobBar(j: Job | undefined, now: Date): ClockBar | null {
  if (!j?.max_duration || !jobRunning(j)) return null;
  const mins = parseDuration(j.max_duration);
  if (mins === null || mins <= 0) return null;
  const used = (j.clock_used ? parseDuration(j.clock_used) : 0) ?? 0;
  if (j.state === "steered") {
    const left = limitLength(Math.max(0, mins - used));
    return {
      fraction: Math.min(1, used / mins),
      start: "Paused while you steer",
      endLabel: "Time left:",
      endValue: left,
      warn: false,
      label: `Job time: paused while you steer, ${left} left of ${limitLength(mins)}`,
    };
  }
  if (j.state !== "running" && j.state !== "blocked") return null;
  const began = Date.parse(j.clock_started_at ?? "");
  if (Number.isNaN(began)) return null;
  const elapsed = used + Math.max(0, now.getTime() - began) / 60_000;
  const start = `${used > 0 ? "Resumed" : "Started"} ${clock(new Date(began).toISOString())}`;
  const endValue = clock(new Date(began + (mins - used) * 60_000).toISOString());
  const at = clock(now.toISOString());
  return {
    fraction: Math.min(1, elapsed / mins),
    start,
    endLabel: "Job stops at",
    endValue,
    now: at,
    warn: (mins - elapsed) * 60 <= WarnWithin,
    label: `Job time: ${start}, now ${at}, stops at ${endValue}`,
  };
}

/**
 * jobRunning reports whether a job is still going to change on its own.
 * Terminal states are listed rather than derived; an unknown state
 * counts as running, since the cost of giving up on a live one is worse
 * than polling a finished one once too often.
 *
 * @decision(DL-057) @decision(DL-045)
 */
export function jobRunning(j: Job | undefined): boolean {
  if (!j) return false;
  return !["succeeded", "failed", "cancelled"].includes(j.state);
}

/**
 * sessionSettling reports whether a session page should ask for its
 * row: true before anything has arrived, since a resource's `when`
 * gates the first load as well as the polling and a gate that only said
 * "starting" would leave the page loading forever. After that only
 * `starting` keeps asking.
 *
 * `failed` is the caller's own flag rather than a state: a request that
 * failed leaves no state at all, which must not be read as "nothing has
 * arrived yet" and poll forever on a session that 404s.
 *
 * @decision(DL-017)
 */
export function sessionSettling(state: string | undefined, failed = false): boolean {
  if (failed) return false;
  if (state === undefined) return true;
  return state === "starting";
}

/**
 * wallClock describes a job's time limit for the sidebar: its length,
 * and either when it runs out or why it is not counting. It counts only
 * while the job runs or waits on a permission, across the whole job: a
 * steered job's clock is paused, and a resume continues with what is
 * left. Null for a job with no limit, from before DL-049, or one that
 * has finished.
 *
 * @decision(DL-049)
 */
export function wallClock(
  j: Job | undefined,
): { length: string; endsAt?: string; note: string } | null {
  if (!j?.max_duration || !jobRunning(j)) return null;
  const mins = parseDuration(j.max_duration);
  if (mins === null) return null;
  const length = limitLength(mins);
  const used = (j.clock_used ? parseDuration(j.clock_used) : 0) ?? 0;
  if ((j.state === "running" || j.state === "blocked") && j.clock_started_at) {
    const start = Date.parse(j.clock_started_at);
    if (!Number.isNaN(start)) {
      return { length, endsAt: new Date(start + (mins - used) * 60_000).toISOString(), note: "" };
    }
  }
  if (j.state === "steered") {
    const left = Math.max(0, mins - used);
    return { length, note: used > 0 ? `Paused while you steer, ${limitLength(left)} left` : "Paused while you steer" };
  }
  if (j.state === "delivering") return { length, note: "The run is over" };
  return { length, note: "Once it starts" };
}

/**
 * limitLength renders a limit the way it was asked for: `2m`, `1h`,
 * `1h 30m`. duration's zero-padded `0h 02m` reads as elapsed time.
 */
export function limitLength(mins: number): string {
  const whole = Math.round(mins);
  const h = Math.floor(whole / 60);
  const m = whole % 60;
  if (h === 0) return `${m}m`;
  return m === 0 ? `${h}h` : `${h}h ${m}m`;
}

/**
 * parseDuration reads the Go durations the API reports ("1h0m0s", "45m",
 * "2h30m") as minutes, and returns null for anything else.
 */
export function parseDuration(s: string): number | null {
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(s.trim());
  if (!m || (m[1] === undefined && m[2] === undefined && m[3] === undefined)) return null;
  return Number(m[1] ?? 0) * 60 + Number(m[2] ?? 0) + Number(m[3] ?? 0) / 60;
}


export type CredentialKind = components["schemas"]["SandboxCredentialView"]["kind"];

/**
 * CREDENTIAL_LINES says, for each kind of credential a sandbox can hold,
 * what the session page shows. A Record over the API's kinds, so a new
 * kind fails `astro check` until it is worded here.
 *
 * @decision(DL-043) @decision(DL-021)
 */
export const CREDENTIAL_LINES: Record<CredentialKind, { label: string; note: string }> = {
  renewed: {
    label: "App token",
    note: "Narrowed to these repositories, and renewed while the session is in use.",
  },
  stored: {
    label: "Stored token",
    note: "Handed over as it is: it reaches whatever it was created to reach, and spov cannot check that.",
  },
};

/**
 * PROXIED_NOTES replace a kind's note when the credential proxy holds the
 * token, which is what narrows a stored token to the session's
 * repositories.
 *
 * @decision(DL-034)
 */
export const PROXIED_NOTES: Record<CredentialKind, string> = {
  renewed:
    "Held by spov's proxy, out of the agent's reach. GitHub narrows it to these repositories, and it is renewed while the session is in use.",
  stored:
    "Held by spov's proxy, out of the agent's reach, and used only for these repositories' code, pull requests and issues.",
};

/** What a GitHub PAT still reaches through the proxy. @decision(DL-034) */
export const PAT_GRAPHQL_NOTE =
  " GitHub's GraphQL API names no repository, so there it reaches whatever it was created to reach.";

/** What a GitLab token still reaches through the proxy. @decision(DL-034) */
export const GITLAB_GRAPHQL_NOTE =
  " GitLab's GraphQL API names no project, so there it reaches every project its account can.";

/** A token the sandbox itself holds. @decision(DL-034) */
export const IN_SANDBOX_NOTE = " The token is inside the sandbox, where the agent can read it.";

/** What a token with git access all reaches, per kind. @decision(DL-043) */
export const ALL_NOTES: Record<CredentialKind, string> = {
  renewed:
    "Reaches every repository the App is installed on (git access all), and is renewed while the session is in use.",
  stored: "Used on every repository it reaches (git access all).",
};

/** What the proxy still limits a stored token to. @decision(DL-043) @decision(DL-034) */
export const PROXIED_ENDPOINTS_NOTE = " The proxy uses it only for code, pull requests and issues.";

/** A token the proxy holds, before what it reaches. @decision(DL-043) */
export const PROXY_HELD = "Held by spov's proxy, out of the agent's reach. ";

/**
 * FELL_BACK_NOTE is added where the launch asked for all and this
 * platform got its repositories.
 *
 * @decision(DL-043)
 */
export const FELL_BACK_NOTE =
  " Git access all was asked for, but this is the deployment's credential, which this deployment lets reach" +
  " only the session's repositories.";

/**
 * credentialLine is the session page's wording for one credential. A kind
 * this build does not know reads as a stored token, the broader claim,
 * and a holder it does not know says nothing about where the token is.
 * `access` is what the platform got and `asked` what the launch asked
 * for.
 *
 * @decision(DL-043) @decision(DL-034)
 */
export function credentialLine(
  kind: string,
  holder?: string,
  forge?: string,
  access?: string,
  asked?: string,
): { label: string; note: string } {
  const known = Object.hasOwn(CREDENTIAL_LINES, kind) ? (kind as CredentialKind) : "stored";
  const line = CREDENTIAL_LINES[known];
  if (access === "all") {
    const where = holder === "proxy" ? PROXY_HELD : "";
    const inside = holder === "sandbox" ? IN_SANDBOX_NOTE : "";
    const endpoints = holder === "proxy" && known === "stored" ? PROXIED_ENDPOINTS_NOTE : "";
    return { label: line.label, note: where + ALL_NOTES[known] + endpoints + inside };
  }
  const fell = asked === "all" ? FELL_BACK_NOTE : "";
  const plain = narrowLine(known, line, holder, forge);
  return { label: plain.label, note: plain.note + fell };
}

/** credentialLine for a token limited to the session's repositories. */
function narrowLine(
  known: CredentialKind,
  line: { label: string; note: string },
  holder?: string,
  forge?: string,
): { label: string; note: string } {
  if (holder === "proxy") {
    const pat = known !== "stored" ? "" : forge === "github" ? PAT_GRAPHQL_NOTE : forge === "gitlab" ? GITLAB_GRAPHQL_NOTE : "";
    return { label: line.label, note: PROXIED_NOTES[known] + pat };
  }
  if (holder === "sandbox") {
    return { label: line.label, note: line.note + IN_SANDBOX_NOTE };
  }
  return line;
}

/**
 * credentialTag is the part of a credential's line shown without asking:
 * its kind, and where it departs from the usual case — every repository
 * rather than the session's, or held inside the sandbox rather than by
 * the proxy. The rest of `credentialLine` is shown on demand.
 *
 * @decision(DL-057) @decision(DL-043) @decision(DL-034) @decision(DL-042)
 */
export function credentialTag(kind: string, holder?: string, access?: string, tier?: string): string {
  const known = Object.hasOwn(CREDENTIAL_LINES, kind) ? (kind as CredentialKind) : "stored";
  let tag = CREDENTIAL_LINES[known].label;
  // Whose credential, when the launch recorded it. @decision(DL-042)
  if (tier === "org") tag += ", the organisation's";
  else if (tier === "user") tag += ", your own";
  if (access === "all") tag += ", every repository";
  if (holder === "sandbox") tag += ", inside the sandbox";
  return tag;
}

/** GoalLimit is how much of a job's goal the sidebar shows folded. */
export const GoalLimit = 240;

/**
 * clip cuts a text to about `limit` characters for a folded view, at a
 * word boundary when one is near, and says whether it cut anything.
 *
 * @decision(DL-057)
 */
export function clip(text: string, limit: number): { head: string; clipped: boolean } {
  const t = text.trim();
  if (t.length <= limit) return { head: t, clipped: false };
  const cut = t.slice(0, limit);
  const space = cut.search(/\s\S*$/);
  const head = space > limit * 0.75 ? cut.slice(0, space) : cut;
  return { head: `${head.trimEnd()}…`, clipped: true };
}

/**
 * verdictSummary is the verdict as one mark and word, for the folded
 * verdict: the worst gate decides it, except that a gate spov could not
 * verify beside ones that passed reads as a pass with that said beside
 * it, since a job with no check passes that way every time.
 *
 * @decision(DL-057) @decision(DL-050)
 */
export function verdictSummary(gates: { outcome: string }[]): { mark: Mark; word: string; open: boolean } {
  const n = (o: string) => gates.filter((g) => g.outcome === o).length;
  const failed = n("failed");
  if (failed) return { mark: "fail", word: failed === 1 ? "1 gate failed" : `${failed} gates failed`, open: true };
  if (n("partial")) return { mark: "part", word: "Partial", open: true };
  const passed = n("passed");
  const other = gates.length - passed;
  if (!passed) return { mark: "stop", word: "Unverified", open: false };
  if (!other) return { mark: "done", word: "Passed", open: false };
  return { mark: "done", word: `Passed, ${other} unverified`, open: false };
}

/** A routed credential's route, as the API carries it. */
export interface RoutedRoute {
  hosts: string[];
  paths?: string[] | null;
  scheme: string;
  user?: string;
  header?: string;
  preset?: string;
}

/**
 * routedCredentialLine writes a routed credential's route out: its hosts,
 * any path prefixes, and how the secret is sent. Never the secret, which
 * no view carries.
 *
 * @decision(DL-077)
 */
export function routedCredentialLine(r: RoutedRoute): string {
  let where = r.hosts.join(", ");
  if (r.paths && r.paths.length > 0) where += " " + r.paths.join(", ");
  const how =
    r.scheme === "basic" ? `Basic as ${r.user ?? ""}` : r.scheme === "header" ? `in ${r.header ?? "a header"}` : "as a bearer";
  return `${where}, ${how}`;
}

/**
 * routedCredentialTag is the routed credential's kind and whose it is.
 *
 * @decision(DL-077)
 */
export function routedCredentialTag(tier?: string, preset?: string): string {
  let tag = preset ? `${preset}, routed` : "routed";
  if (tier === "org") tag += ", the organisation's";
  else if (tier === "user") tag += ", your own";
  return tag;
}

/**
 * envRows are a session's own environment variables in name order, as
 * the sidebar lists them.
 *
 * @decision(DL-096)
 */
export function envRows(env?: Record<string, string> | null): { name: string; value: string }[] {
  return Object.keys(env ?? {})
    .sort()
    .map((name) => ({ name, value: env![name] }));
}
