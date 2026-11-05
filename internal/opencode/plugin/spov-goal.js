// spov-goal — the loop that keeps an autonomous job running, and the
// guardrail on what its agent may do.
//
// Written into $HOME/.config/opencode/plugins/ by the entrypoint. It
// imports no OpenCode package: the published one's V2 context is
// narrower than the in-repo one and cannot drive a loop, and a plain
// object default export is all the loader decodes.
//
// The config token below is replaced by the server with a JSON document
// before the file is written into the sandbox.

const CONFIG = SPOV_GOAL_CONFIG

let fs
try {
  fs = await import("node:fs")
} catch (e) {
  // Without this the evidence file never appears and the only symptom
  // is a missing record, which reads as "the primitive did not attach"
  // rather than "the recorder is broken".
  try { console.log("SPOV_GOAL evidence unavailable: " + String(e)) } catch (e2) {}
}

// matchRepo finds a selected repository by owner/name, or by
// host/owner/name when two share a name.
//
// @decision(DL-051)
function matchRepo(given) {
  const name = given.trim().replace(/^https?:\/\//, "").replace(/\.git$/, "").replace(/\/+$/, "")
  const listed = CONFIG.repos.map((r) => r.slug + " (" + r.host + ")").join(", ")
  // Compared without case; what is recorded is always the configured slug.
  const want = name.toLowerCase()
  const exact = CONFIG.repos.filter((r) => (r.host + "/" + r.slug).toLowerCase() === want)
  if (exact.length === 1) return { ok: true, platform: exact[0].platform, slug: exact[0].slug }
  const bySlug = CONFIG.repos.filter((r) => r.slug.toLowerCase() === want)
  if (bySlug.length === 1) return { ok: true, platform: bySlug[0].platform, slug: bySlug[0].slug }
  if (bySlug.length > 1) {
    return { ok: false, reason: "More than one repository is called " + name +
      ". Give it as host/owner/name. The repositories are: " + listed + "." }
  }
  return { ok: false, reason: "Nothing was recorded: " + JSON.stringify(given) + " is not one of " +
    "this job's repositories, and a job can only deliver to those. They are: " + listed + "." }
}

// The events that decide a job's outcome, copied to CONFIG.outcomePath so
// they are never lost to the evidence file's size bound.
//
// @decision(DL-051) @decision(DL-050)
const OUTCOME_EVENTS = new Set(["loop.stopped", "completion.declared", CONFIG.checkEvent, CONFIG.deliverableEvent])

// Evidence is written to a file rather than sent into the session: a
// reply that starts no turn sits in the sandbox's own store until the
// next turn ends, so it is not a channel anything may wait on.
function record(event, data) {
  const line = JSON.stringify({ event, at: new Date().toISOString(), ...data })
  try { console.log("SPOV_GOAL " + line) } catch (e) {}
  try { fs.appendFileSync(CONFIG.evidencePath, line + "\n") } catch (e) {}
  if (OUTCOME_EVENTS.has(event) && CONFIG.outcomePath) {
    try { fs.appendFileSync(CONFIG.outcomePath, line + "\n") } catch (e) {}
  }
}

function describe(value, n) {
  try { return JSON.stringify(value).slice(0, n ?? 400) } catch (e) { return String(value) }
}

// The loop's own state. `paused` and `stopped` are set by /spov-goal and
// are the plugin's notion of paused, which is not the job row's state:
// only the supervisor writes those.
const loop = {
  paused: false,
  stopped: false,
  // Why the loop stopped, set before the announcement so a later
  // announce (no session id at declare time) still reports the real
  // reason rather than an ordinary stop.
  stopReason: null,
  continuations: 0,
  turns: 0,
  goalShapeReported: false,
  announced: false,
  checked: false,
  // Whether a turn is executing right now: set by
  // session.execution.started and cleared by every way a turn can end.
  // A resume must not nudge during one — the turn already in flight
  // ends and sends its own continuation, so a second would be billed
  // for nothing.
  inFlight: false,
  // The call whose repeated denial ended the run (DL-040), named in
  // the stop marker.
  deniedCall: null,
  // Turns that failed in a row, and the error of the one that ended the
  // run (DL-048), named in the stop marker.
  failures: 0,
  failedError: null,
  // The session this job's turns run in, learned from the first
  // execution event. A subagent's session is not this one, and its
  // turns must not move the loop's own in-flight flag.
  session: null,
}

// What a pause says, to both audiences that read it: the loop stops,
// but the turn already in flight does not, and the agent is asked
// directly to stop and take no new action. Names no tool, since an
// agent reaching for the completion tool would end the job rather than
// suspend it.
//
// @decision(DL-039)
const PAUSE_NOTICE =
  "spov-goal: paused. The loop will not send another continuation, but this " +
  "does not stop work already running.\n\n" +
  "To the agent: you are being paused by the person watching this session. " +
  "Finish the tool call you are in, then stop and take no new action — do not " +
  "start further work, and do not declare the goal complete. Someone will " +
  "resume you or tell you what to do next."

// What the completion tool answers while a person's message is waiting.
//
// @decision(DL-048)
const DEFERRED_COMPLETION =
  "Not yet: a message from the person watching this job is waiting and will reach you " +
  "next. Read it and act on it, then call this tool again when the goal is met. " +
  "Nothing from this call was recorded."

// The token the supervisor matches to learn that the run has ended. It
// is a literal shared with the Go side (opencode.LoopStoppedMarker) and
// changing it in one place alone strands every job at `running`.
const STOP_MARKER = "SPOV_LOOP_STOPPED"

// The rule internal/jobs' takesOver applies, in the plugin: an enqueued
// item typed "user" whose spov metadata does not say "continue". An item
// with no metadata (the attached TUI) takes over. The item is found the
// way InboxItemType finds it, so the two agree on every frame
// (TestThePluginAgreesOnWhatTakesOver).
//
// @decision(DL-052) @decision(DL-046)
const MAX_PROJECT_DEPTH = 5

const AFTER_CONTINUE = "continue"

function findKey(v, key, depth, want) {
  if (depth >= MAX_PROJECT_DEPTH || !isObject(v)) return null
  if (want(v[key])) return v[key]
  for (const k of Object.keys(v).sort()) {
    if (!isObject(v[k])) continue
    const found = findKey(v[k], key, depth + 1, want)
    if (found !== null) return found
  }
  return null
}

function findItem(v, depth) {
  return findKey(v, "item", depth, isObject)
}

function isObject(v) {
  return v !== null && typeof v === "object" && !Array.isArray(v)
}

function eventType(ev) {
  return ev?.type ?? ev?.event
}

// inboxIDOf is the inbox item an inbox event is about, as internal/jobs'
// InboxID reads it.
//
// @decision(DL-052)
function inboxIDOf(ev) {
  return findKey(ev, "inboxID", 0, (v) => typeof v === "string" && v !== "") ?? ""
}

function isUserInbox(ev) {
  if (eventType(ev) !== "session.inbox.enqueued") return false
  return findItem(ev, 0)?.type === "user"
}

function takesOver(ev) {
  if (!isUserInbox(ev)) return false
  const after = findItem(ev, 0)?.payload?.metadata?.spov?.after
  return after !== AFTER_CONTINUE
}

// The person's messages this plugin has seen enqueued and not yet
// delivered or cancelled, and which of them take the job over once
// delivered. Ids only, never text.
//
// @decision(DL-052) @decision(DL-048)
const inbox = {
  pending: new Set(),
  takeovers: new Set(),
}

// trackInbox follows one inbox event, and reports whether it was the
// delivery of a takeover item.
//
// @decision(DL-052) @decision(DL-048)
function trackInbox(ev) {
  const type = eventType(ev)
  if (typeof type !== "string" || !type.startsWith("session.inbox.")) return false
  const id = inboxIDOf(ev)
  if (!id) return false
  switch (type) {
    case "session.inbox.enqueued":
      if (isUserInbox(ev)) inbox.pending.add(id)
      if (takesOver(ev)) inbox.takeovers.add(id)
      return false
    case "session.inbox.delivered": {
      inbox.pending.delete(id)
      const held = inbox.takeovers.delete(id)
      return held
    }
    case "session.inbox.cancelled":
      inbox.pending.delete(id)
      inbox.takeovers.delete(id)
      return false
  }
  return false
}

// A continuation goes out as a synthetic, never as a prompt: a prompt
// arrives indistinguishable from a human's and would move the job to
// steered on its own first turn.
async function continueRun(ctx, sessionID) {
  // Counted for the status reply and the evidence log; no count ends a
  // run. A continuation that cannot be sent ends it instead.
  //
  // @decision(DL-049)
  loop.continuations += 1
  // Queued, so it never goes ahead of a message a person queued first.
  //
  // @decision(DL-052)
  await ctx.session.synthetic({
    sessionID,
    text: CONFIG.continueText,
    resume: true,
    delivery: "queue",
  })
}

// turnFailed handles a turn that failed: the model or its provider
// refused, or the turn broke. It is retried after each of
// CONFIG.turnRetryDelays, on a timer so the event loop keeps reading
// and a takeover in the meantime still holds the loop; one more failure
// in a row ends the run with the error. A succeeded turn resets the
// count.
//
// @decision(DL-048)
function turnFailed(ctx, ev) {
  if (loop.stopped || loop.paused) return
  const sessionID = ev?.data?.sessionID ?? ev?.properties?.sessionID ?? loop.session
  if (!sessionID) return
  const err = ev?.data?.error ?? ev?.properties?.error ?? {}
  const error = String(err?.message ?? err?.type ?? "the turn failed").slice(0, 1000)
  loop.failures += 1
  record("turn.failed", { error, type: String(err?.type ?? ""), status: err?.status ?? null, failures: loop.failures })
  const delays = CONFIG.turnRetryDelays ?? []
  if (loop.failures > delays.length) {
    loop.failedError = error
    loop.stopReason = CONFIG.turnFailedReason
    announceStop(ctx, sessionID, CONFIG.turnFailedReason).catch((e) =>
      record("turn.stop.failed", { error: String(e) }))
    return
  }
  const failures = loop.failures
  setTimeout(() => {
    if (loop.stopped || loop.paused || loop.inFlight || loop.failures !== failures) {
      record("turn.retry.skipped", { failures })
      return
    }
    continueRun(ctx, sessionID).then(
      () => record("turn.retried", { failures }),
      (e) => {
        record("continue.failed", { error: String(e) })
        return announceStop(ctx, sessionID, "continue-failed")
      },
    ).catch((e) => record("turn.retry.failed", { error: String(e) }))
  }, delays[failures - 1])
}

// nudge restarts a loop that has just been released, for a session with
// no turn of its own to fire on. The resume's half of DL-054.
//
// @decision(DL-054) @decision(DL-039)
async function nudge(ctx, sessionID) {
  if (!sessionID) {
    record("nudge.skipped", { reason: "no-session" })
    return
  }
  // Nothing to restart. A stop is final by DL-039, and a loop still
  // paused means the resume did not take.
  if (loop.stopped || loop.paused) {
    record("nudge.skipped", { reason: loop.stopped ? "stopped" : "paused" })
    return
  }
  if (loop.inFlight) {
    record("nudge.skipped", { reason: "in-flight" })
    return
  }
  try {
    await continueRun(ctx, sessionID)
    record("nudge.sent", { continuations: loop.continuations })
  } catch (e) {
    record("nudge.failed", { error: String(e) })
    // Same reasoning as the event loop's own failed continuation: a
    // continuation that cannot be sent ends the run as surely as one
    // never attempted, and nothing else will announce it.
    await announceStop(ctx, sessionID, "continue-failed")
  }
}

// Replies travel as session content because the command endpoint answers
// 204 and runs the handler asynchronously, so it cannot return anything.
// resume: false starts no turn.
async function reply(ctx, sessionID, text) {
  try {
    await ctx.session.synthetic({ sessionID, text, resume: false })
  } catch (e) {
    record("reply.failed", { error: String(e) })
  }
}

// The loop announces its own end, the one hard completion signal a job
// has, as a synthetic with resume: false so it starts no turn and is
// never mistaken for a human steering the session (DL-048). Sent at
// most once, since the event subscription can deliver more turns after
// the loop has stopped.
async function announceStop(ctx, sessionID, reason) {
  if (loop.announced) return
  loop.announced = true
  // The loop is over whatever brought it here. Without this a
  // continuation that FAILED to send would announce the end and then
  // continue on the next turn anyway — the agent spending on a job the
  // server has already delivered and collected.
  loop.stopped = true
  // The check runs before the marker goes out: the supervisor collects
  // evidence in the same visit the marker triggers (DL-048), so a check
  // recorded afterwards is never read.
  //
  // @decision(DL-050)
  try {
    await runCheck()
  } catch (e) {
    record("check.failed", { error: String(e) })
  }
  const payload = {
    reason,
    continuations: loop.continuations,
    turns: loop.turns,
  }
  // @decision(DL-040)
  if (loop.deniedCall) payload.call = loop.deniedCall
  // @decision(DL-048)
  if (loop.failedError) payload.error = loop.failedError
  record("loop.stopped", payload)
  // The marker is machine-readable and prefixed, so the supervisor
  // matches on the prefix rather than on prose the model could produce
  // by accident.
  let line
  try {
    line = STOP_MARKER + " " + JSON.stringify(payload)
  } catch (e) {
    line = STOP_MARKER + ' {"reason":"' + String(reason) + '"}'
  }
  await reply(ctx, sessionID, line)
}

// Gate 1 of DL-050: the repo's own check, run once when the loop stops.
// It runs here, in the sandbox, spawning the command directly rather
// than asking the agent — and outside the permission hook, since the
// hook guards the agent and this is not the agent.
async function runCheck() {
  if (!CONFIG.checkCommand) {
    // No check configured. Gate 1 is then unverified rather than
    // failed, which is the server's reading of a missing record.
    return
  }
  if (loop.checked) return
  loop.checked = true

  let spawn
  try {
    ;({ spawn } = await import("node:child_process"))
  } catch (e) {
    record(CONFIG.checkEvent, { infra: true, command: CONFIG.checkCommand, output: "cannot spawn: " + String(e) })
    return
  }

  const started = Date.now()
  try {
    const result = await new Promise((resolve) => {
      // Through a shell, since the check is a command line rather than
      // an argv. detached, so the shell leads its own process group:
      // killing the shell alone leaves an orphan holding the inherited
      // stdout/stderr pipes and the sandbox open. cwd is sent, never
      // inherited from the opencode server's own (DL-037's workspace
      // root rather than a repository).
      const child = spawn("/bin/sh", ["-c", CONFIG.checkCommand], {
        stdio: ["ignore", "pipe", "pipe"],
        detached: true,
        cwd: CONFIG.dir || undefined,
      })
      // Bounded, and the output is kept from the END: a failing suite
      // says why in its last lines, and keeping the head would store
      // the banner and drop the reason.
      let out = ""
      const keep = (chunk) => {
        out += String(chunk)
        if (out.length > 16000) out = out.slice(-16000)
      }
      child.stdout?.on("data", keep)
      child.stderr?.on("data", keep)

      let timer = null
      let settled = false
      const finish = (value) => {
        if (settled) return
        settled = true
        if (timer !== null) clearTimeout(timer)
        resolve(value)
      }
      timer = setTimeout(() => {
        // Killed, and recorded as a TIMEOUT rather than as an exit
        // code. DL-050 needs the two distinguishable so a slow suite
        // is not read as a broken one; a killed process reports a
        // signal exit that looks like an ordinary failure.
        // The whole group, not just the shell: a negative pid is the
        // group, which is what actually stops what the check started.
        try { process.kill(-child.pid, "SIGKILL") } catch (e) {
          try { child.kill("SIGKILL") } catch (e2) {}
        }
        // The pipes are dropped as well as the process. An orphan that
        // somehow survives the group kill would otherwise keep them
        // open and hold the event loop; unref'ing means the plugin
        // does not wait on it either way.
        try { child.stdout?.destroy() } catch (e) {}
        try { child.stderr?.destroy() } catch (e) {}
        try { child.unref() } catch (e) {}
        finish({ timeout: true, output: out })
      }, CONFIG.checkTimeoutMs)

      child.on("error", (e) => finish({ infra: true, output: String(e) }))
      child.on("close", (code, signal) => {
        if (signal) {
          finish({ timeout: false, infra: true, output: out + "\nkilled by " + String(signal) })
          return
        }
        finish({ exit: typeof code === "number" ? code : 1, output: out })
      })
    })
    record(CONFIG.checkEvent, {
      ...result,
      command: CONFIG.checkCommand,
      durationMs: Date.now() - started,
    })
  } catch (e) {
    record(CONFIG.checkEvent, { infra: true, command: CONFIG.checkCommand, output: String(e) })
  }
}

function statusText() {
  return [
    "spov-goal: " + (loop.stopped ? "stopped" : loop.paused ? "paused" : "running"),
    "continuations: " + loop.continuations,
    "turns seen: " + loop.turns,
  ].join("\n")
}

// The incoming effect is "allow" or "ask": the sandbox is permissive by
// default, so the policy is an allowlist over an allow-all baseline.
// Anything unmatched is denied — a denylist here would inherit the
// baseline. An incoming "deny" never reaches this; the hook keeps it.
//
// The subject is built untruncated. Scanning a truncated one lets a
// command longer than the cut-off reach a denied path in its tail and
// still be allowed by its action; truncation belongs in the evidence
// log, which is the only place it is needed.
function decide(evaluation) {
  const action = String(evaluation?.action ?? "")
  let subject
  try {
    subject = action + " " + JSON.stringify(evaluation?.resources ?? null)
  } catch (e) {
    // Unreadable resources cannot be scanned for a denied path, and an
    // unscannable subject is not evidence that it is safe.
    record("permission.unreadable", { action, error: String(e) })
    return "deny"
  }
  for (const rule of CONFIG.deny) {
    if (subject.includes(rule)) return "deny"
  }
  // Case-folded: the allowlist names are spov's own spelling and the
  // vocabulary OpenCode emits is not measured, so a difference in case
  // alone must not deny every call.
  const folded = action.toLowerCase()
  for (const rule of CONFIG.allow) {
    if (folded === rule) return "allow"
  }
  // @decision(DL-040)
  if (folded === "edit" && writesInsideRoots(evaluation?.resources)) return "allow"
  return "deny"
}

// writesInsideRoots is DL-040's path grant: true only when there are
// resources and every one resolves inside one of CONFIG.writeRoots. A
// resource inside the session's directory arrives relative to it, one
// outside it arrives absolute (spike S-W1).
//
// @decision(DL-040)
function writesInsideRoots(resources) {
  const roots = CONFIG.writeRoots ?? []
  if (roots.length === 0 || !Array.isArray(resources) || resources.length === 0) return false
  for (const r of resources) {
    const p = resolvePath(CONFIG.dir, r)
    if (p === null) return false
    if (!roots.some((root) => p === root || p.startsWith(root + "/"))) return false
  }
  return true
}

// resolvePath resolves p against dir and removes . and .. segments, so
// the result can be compared as a string. null for anything but a
// non-empty string.
//
// @decision(DL-040)
function resolvePath(dir, p) {
  if (typeof p !== "string" || p === "" || p.includes("\0")) return null
  const full = p.startsWith("/") ? p : (dir || "/") + "/" + p
  const out = []
  for (const part of full.split("/")) {
    if (part === "" || part === ".") continue
    if (part === "..") out.pop()
    else out.push(part)
  }
  return "/" + out.join("/")
}

// The repeated-denial guard's state: the call last denied, how many
// times in a row, and the calls already noted.
//
// @decision(DL-040)
const denials = { key: null, count: 0, noted: new Set() }

// onDecision counts a denied call repeating with no allow between, sends
// one note at CONFIG.denialNoteAt and ends the run at CONFIG.denialStopAt.
// An allowed external_directory does not break the run: it precedes the
// edit of any path outside the session's directory (spike S-W1). Neither
// send is awaited, since the hook decides synchronously, and neither
// happens while the loop is paused for a person.
//
// @decision(DL-040)
function onDecision(ctx, evaluation, want) {
  const action = String(evaluation?.action ?? "")
  if (want === "allow") {
    if (action.toLowerCase() !== "external_directory") {
      denials.key = null
      denials.count = 0
    }
    return
  }
  let key
  try {
    key = action + " " + JSON.stringify(evaluation?.resources ?? null)
  } catch (e) {
    key = action + " (unreadable)"
  }
  if (key === denials.key) denials.count += 1
  else {
    denials.key = key
    denials.count = 1
  }
  if (loop.stopped || loop.paused) return
  // The note goes where the call was made; the stop goes to the job's
  // own session, which a subagent's call is not.
  const noteTo = evaluation?.sessionID ?? loop.session
  const stopTo = loop.session ?? evaluation?.sessionID
  const shown = key.slice(0, 300)
  if (noteTo && denials.count >= CONFIG.denialNoteAt && !denials.noted.has(key)) {
    denials.noted.add(key)
    record("denial.noted", { call: shown, count: denials.count })
    reply(ctx, noteTo, "spov: this call has been denied " + denials.count +
      " times in a row: " + shown + ". " + CONFIG.denialNote)
  }
  if (stopTo && denials.count >= CONFIG.denialStopAt) {
    loop.stopped = true
    loop.stopReason = CONFIG.deniedReason
    // Longer than what is shown: the server redacts before it cuts, so
    // a token is never cut below the length its pattern needs.
    loop.deniedCall = key.slice(0, 4000)
    record("denial.stopped", { call: shown, count: denials.count })
    announceStop(ctx, stopTo, CONFIG.deniedReason).catch((e) =>
      record("denial.stop.failed", { error: String(e) }))
  }
}

export default {
  id: "spov.goal",
  async setup(ctx) {
    record("setup", { kind: CONFIG.kind, domains: Object.keys(ctx ?? {}).sort() })

    // The goal is pushed onto system[] on every request, and onto the
    // compaction summariser's, which GA calls through its own hook
    // (DL-039). Whether compaction drops it is not stable between runs,
    // so there is no trigger worth reacting to and nothing worth
    // checking for.
    const pushGoal = (event) => {
      const system = event?.system
      if (!Array.isArray(system)) return
      // system[] holds {type: "text", text} objects on both the beta and
      // GA. An entry of a shape this does not recognise is skipped, and
      // the agent then reports having no standing goal.
      //
      // A string is pushed when the existing entries are strings, and
      // an object shaped from a neighbour when they are objects, so
      // this survives the build changing under it in either
      // direction. An empty array takes the string form.
      const first = system[0]
      let entry = null
      if (typeof first === "string" || first === undefined) {
        entry = CONFIG.goalText
      } else if (first && typeof first === "object") {
        if (typeof first.text === "string") entry = { ...first, text: CONFIG.goalText }
        else if (typeof first.content === "string") entry = { ...first, content: CONFIG.goalText }
      }
      if (entry === null) {
        // The goal silently stops being re-asserted from here on, so
        // the shape that defeated it is worth recording once — and
        // said out loud, since the evidence file is not collected for
        // a job that is cancelled.
        if (!loop.goalShapeReported) {
          loop.goalShapeReported = true
          record("goal.push.skipped", { reason: "unrecognised system entry", sample: describe(first, 200) })
          try {
            console.log("SPOV_GOAL the goal could not be injected: system[0] is " +
              describe(first, 200) + " — the agent will report having no standing goal")
          } catch (e) {}
        }
        return
      }
      try { system.push(entry) } catch (e) { record("goal.push.failed", { error: String(e) }) }
    }
    for (const name of ["context", "compaction"]) {
      try {
        ctx.session.hook(name, pushGoal)
        record("goal.hooked", { hook: name })
      } catch (e) {
        record("goal.failed", { hook: name, error: String(e) })
      }
    }

    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        // A deny that reached the hook (an organisation policy, say) is
        // never relaxed; the policy decides only allow and ask.
        //
        // @decision(DL-039)
        const incoming = String(evaluation?.effect) === "deny"
        const want = incoming ? "deny" : decide(evaluation)
        try {
          evaluation.effect = want
        } catch (e) {
          // The hook has no other channel, so a failed write leaves the
          // incoming "allow" standing: this path is fail-OPEN and there
          // is nothing here that can close it. It is recorded at high
          // signal so a verdict can see the policy did not apply.
          record("permission.not.applied", {
            wanted: want,
            action: String(evaluation?.action ?? ""),
            error: String(e),
          })
          return
        }
        if (evaluation.effect !== want) {
          record("permission.not.applied", {
            wanted: want,
            got: String(evaluation?.effect),
            action: String(evaluation?.action ?? ""),
          })
        }
        // Every evaluated action is recorded with the verb that decided
        // it, allow included. The allowlist is matched against a
        // vocabulary that has never been measured, so a policy that
        // denies everything and one that is working look identical
        // without this: both simply run and produce nothing.
        record(want === "allow" ? "permission.allowed" : "permission.denied", {
          action: String(evaluation?.action ?? ""),
          resources: describe(evaluation?.resources, 400),
          source: describe(evaluation?.source, 200),
          ...(incoming ? { incoming: true } : {}),
        })
        // @decision(DL-040)
        try {
          onDecision(ctx, evaluation, want)
        } catch (e) {
          record("denial.guard.failed", { error: String(e) })
        }
      })
      record("permission.hooked", {})
    } catch (e) {
      record("permission.failed", { error: String(e) })
    }

    // The completion tool: how a job that has done its work ends,
    // rather than a phrase the plugin watches for — a tool call cannot
    // be produced by accident or forged by a human typing the same
    // words, the same invariant that makes a continuation a synthetic
    // rather than a prompt. Declaring completion ends the run, not the
    // job's outcome; DL-050's gates still judge what it left behind.
    //
    // The tool asserts no permission action (measured, spike S10), so
    // the registration and any failure of it are recorded and said on
    // stdout too, since a cancelled job's evidence file is never
    // collected.
    //
    // @decision(DL-048)
    try {
      ctx.tool.transform((draft) => {
        draft.add({
          name: CONFIG.completionTool,
          // codemode: false makes this a DIRECT tool call rather than
          // one reachable only from generated code, and it is required
          // rather than a preference: the toolset splits on it, and the
          // default lands in the codemode catalog, reachable only from
          // generated code. Every built-in declares codemode: false.
          options: { codemode: false },
          // The description states this is the LAST action, not merely
          // that work should be done first: calling it begins delivery
          // within seconds, and the plugin cannot stop the turn already
          // in flight, so anything done afterward is at best
          // uncollected and at worst killed half-done. A guardrail on
          // an honest run, not a boundary.
          //
          // @decision(DL-048)
          description:
            "Declare that the standing goal for this session has been met, ending the job. " +
            "This must be your LAST action: calling it immediately runs the repo's check, " +
            "collects the deliverable and shuts the sandbox down, so anything you do afterwards " +
            "is not collected and may be cut off part-way. Finish every file write, commit and " +
            "push BEFORE calling this. It is the only way to end the run, and stating completion " +
            "in a reply does nothing.",
          input: {
            type: "object",
            properties: {
              summary: {
                type: "string",
                description:
                  "A short account of what was accomplished, for the human reading this job's report.",
              },
              // Evidence a person reads, as summary is. Its only effect
              // on the verdict is to make a success partial.
              //
              // @decision(DL-050)
              unfinished: {
                type: "string",
                description:
                  "Anything the goal asked for that you know you did not finish, and why. " +
                  "Leave it out when everything is done. Saying so is not a failure: it keeps " +
                  "the job's result honest.",
              },
            },
            required: ["summary"],
            additionalProperties: false,
          },
          // No `output` schema is declared, matching every built-in:
          // with output undefined the dispatcher takes the return's
          // `content` and refuses a return carrying `output`.
          execute: async (input, context) => {
            // The summary is evidence, never control: no gate reads it.
            const summary = String(input?.summary ?? "").slice(0, 4000)
            // @decision(DL-050)
            const unfinished = String(input?.unfinished ?? "").trim().slice(0, 4000)
            const sessionID = context?.sessionID ?? input?.sessionID
            // A person's message still waiting for this turn to end is
            // read before the job ends. Nothing from this call counts:
            // no check runs and no stop is announced.
            //
            // @decision(DL-048)
            if (inbox.pending.size > 0 && !loop.stopped) {
              record("completion.deferred", { pending: inbox.pending.size })
              return { content: DEFERRED_COMPLETION }
            }
            record("completion.declared", {
              summary,
              unfinished,
              sessionID: String(sessionID ?? ""),
            })
            loop.stopped = true
            loop.stopReason = "declared"
            if (sessionID) {
              // Awaited inside the tool call, blocking the agent's turn
              // for as long as the check runs: the supervisor collects
              // evidence in the same visit the marker triggers, so a
              // check still running when the marker goes out is a
              // check nobody reads.
              await announceStop(ctx, sessionID, "declared")
            } else {
              // Without a session there is nothing to announce into;
              // the event subscription announces on the next turn's
              // end, which is why loop.stopped is set above regardless.
              record("completion.no.session", { keys: describe(Object.keys(context ?? {}), 200) })
            }
            // The return MUST be an object carrying `content`: a bare
            // string throws in the dispatcher, and the errored part is
            // then read by gate 3 as a refusal.
            //
            // The codemode path does not go through that check, so a
            // string passes there and fails once the tool is a direct
            // call.
            return { content: "Goal declared complete. The job is ending; no further work is needed." }
          },
        })
      })
      record("completion.tool.registered", { name: CONFIG.completionTool })
    } catch (e) {
      record("completion.tool.failed", { name: CONFIG.completionTool, error: String(e) })
      // With no way to declare completion, nothing but the wall clock
      // would end the run, so the loop stops after the opening turn and
      // the job is judged on that.
      //
      // @decision(DL-049) @decision(DL-048)
      loop.stopped = true
      loop.stopReason = "no-completion-tool"
      try {
        console.log("SPOV_GOAL the completion tool could not be registered: " + String(e) +
          " — the run stops after its first turn")
      } catch (e2) {}
    }

    // A pr job records each pull request as it opens it, so the
    // inventory exists however the run ends. Recording ends nothing.
    // The repository must be one the job was launched with; the server
    // checks that again, since the evidence file is the agent's to write.
    //
    // @decision(DL-051)
    if (CONFIG.pullRequestTool) {
      try {
        ctx.tool.transform((draft) => {
          draft.add({
            name: CONFIG.pullRequestTool,
            options: { codemode: false },
            description:
              "Record a pull request (on GitLab, a merge request) you have opened as part of " +
              "this job's result. Call it right after opening each one, once per pull request. " +
              "A pull request you do not record is not part of the result. This does not end the job. " +
              "If the reader asks you to drop one you recorded, call it again with withdrawn: true.",
            input: {
              type: "object",
              properties: {
                repository: {
                  type: "string",
                  description:
                    "The repository the pull request is in, as its whole path (owner/name, or a " +
                    "GitLab project's group/subgroup/name), exactly as listed in your workspace. " +
                    "Only those repositories can be recorded.",
                },
                branch: {
                  type: "string",
                  description:
                    "The branch the pull request is from, with no remote prefix.",
                },
                // @decision(DL-051)
                withdrawn: {
                  type: "boolean",
                  description:
                    "Set to true to take back a pull request you recorded earlier, because the reader " +
                    "asked you to drop it. It counts only once the reader has written to you after you recorded it.",
                },
              },
              required: ["repository", "branch"],
              additionalProperties: false,
            },
            execute: async (input) => {
              const repo = matchRepo(String(input?.repository ?? ""))
              // Trimmed of a remote prefix the agent may include out of
              // habit: the forge matches on the string.
              const branch = String(input?.branch ?? "")
                .trim()
                .replace(/^origin\//, "")
                .slice(0, 255)
              if (!repo.ok) {
                return { content: repo.reason }
              }
              if (!branch) {
                return { content: "Give the branch the pull request is from." }
              }
              // @decision(DL-051)
              if (input?.withdrawn === true) {
                record(CONFIG.deliverableEvent, {
                  platform: repo.platform,
                  repository: repo.slug,
                  branch,
                  withdrawn: true,
                })
                return {
                  content: "Withdrew the pull request from " + branch + " in " + repo.slug +
                    ". spov leaves it out of the result if the reader wrote to you after you recorded it.",
                }
              }
              record(CONFIG.deliverableEvent, {
                platform: repo.platform,
                repository: repo.slug,
                branch,
              })
              return {
                content: "Recorded the pull request from " + branch + " in " + repo.slug +
                  ". spov checks it on the forge when the job ends.",
              }
            },
          })
        })
        record("pull.request.tool.registered", { name: CONFIG.pullRequestTool })
      } catch (e) {
        record("pull.request.tool.failed", { name: CONFIG.pullRequestTool, error: String(e) })
      }
    }

    try {
      ctx.command.transform((draft) => {
        draft.add({
          name: CONFIG.command,
          description: "Inspect and control the spov job loop",
          execute: async (input) => {
            const sessionID = input?.sessionID
            const arg = String(input?.prompt?.text ?? input?.prompt ?? "").trim().toLowerCase()
            switch (arg) {
              case "pause":
                loop.paused = true
                record("command.pause", {})
                await reply(ctx, sessionID, PAUSE_NOTICE)
                return
              case "resume":
                loop.paused = false
                record("command.resume", {})
                await reply(ctx, sessionID, "spov-goal: resumed.")
                // Clearing the flag is not enough: on an idle session
                // there may never be another turn to fire the loop's
                // own continuation, so resume sends one itself. An
                // in-flight turn is skipped since it sends its own
                // continuation.
                //
                // @decision(DL-054)
                await nudge(ctx, sessionID)
                return
              case "stop":
                loop.stopped = true
                record("command.stop", {})
                await reply(ctx, sessionID, "spov-goal: stopped. The loop will not continue again.")
                // Announced here rather than waiting for the next turn
                // to end: on an idle session no turn may ever end, and
                // the job would sit at running until its sandbox idled
                // out and the server's backstop noticed.
                await announceStop(ctx, sessionID, "stopped")
                return
              // spov's own stop at the job's wall clock: final like
              // `stop`, and announced with its own reason so nobody
              // reads it as a person's.
              //
              // @decision(DL-049)
              case "bound":
                loop.stopped = true
                loop.stopReason = "wall-clock"
                record("command.bound", {})
                await reply(ctx, sessionID, "spov-goal: stopped at the job's wall clock. The loop will not continue again.")
                await announceStop(ctx, sessionID, "wall-clock")
                return
              default:
                record("command.status", { arg })
                await reply(ctx, sessionID, statusText())
                return
            }
          },
        })
      })
      record("command.registered", {})
    } catch (e) {
      record("command.failed", { error: String(e) })
    }

    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          // A person's takeover message holds the loop when it is
          // delivered, which is before its execution's succeeded, so no
          // continuation follows it. It sends nothing: the supervisor's
          // own pause brings the notice, and a resume clears this hold
          // as it clears that pause.
          //
          // Another session's inbox (a subagent's) is not this job's, once
          // the job's own session is known.
          //
          // @decision(DL-052)
          const inboxSession = ev?.data?.sessionID ?? ev?.properties?.sessionID
          const foreign = loop.session && inboxSession && inboxSession !== loop.session
          if (!foreign && trackInbox(ev)) {
            if (!loop.paused && !loop.stopped) {
              loop.paused = true
              record("loop.held", { reason: "human-message" })
            }
            continue
          }
          // Tracked so a resume can tell an idle session from one
          // already working; the loop still continues from .succeeded
          // alone. Scoped to the job's own session: a subagent's
          // execution events, if OpenCode emits any onto this stream,
          // must not clear the flag while the parent turn is still
          // running. The session is learned from the first execution
          // event seen, since the plugin is never told which session
          // it belongs to.
          //
          // @decision(DL-054)
          const execSession = ev?.data?.sessionID ?? ev?.properties?.sessionID
          if (type.startsWith("session.execution.")) {
            if (!loop.session && execSession) loop.session = execSession
            if (loop.session && execSession && execSession !== loop.session) continue
          }
          if (type === "session.execution.started") {
            loop.inFlight = true
            continue
          }
          // A turn ends three ways and only one continues the loop, but
          // inFlight must be cleared on all three, or a flag stuck true
          // from an interrupted turn makes every later resume skip its
          // nudge as "in-flight".
          //
          // @decision(DL-054)
          if (type === "session.execution.interrupted" || type === "session.execution.failed") {
            loop.inFlight = false
            // A person's interrupt takes the job over, and Interrupt
            // resumes what they sent: held here, so the execution that
            // answers it sends no continuation before the supervisor's
            // pass. A tool-call reject is reason "shutdown" and holds
            // nothing.
            //
            // @decision(DL-053)
            const reason = ev?.data?.reason ?? ev?.properties?.reason
            if (type === "session.execution.interrupted" && reason === "user" && !loop.paused && !loop.stopped) {
              loop.paused = true
              record("loop.held", { reason: "interrupted" })
            }
            // @decision(DL-048)
            if (type === "session.execution.failed") turnFailed(ctx, ev)
            continue
          }
          if (type !== "session.execution.succeeded") continue
          loop.inFlight = false
          loop.failures = 0
          // Every queued item has been delivered by the time an execution
          // succeeds (DL-019), so anything still counted was a delivery
          // this subscription missed, and must not defer completion for
          // the rest of the run.
          //
          // @decision(DL-048)
          if (inbox.pending.size > 0) {
            record("inbox.reconciled", { dropped: inbox.pending.size })
            inbox.pending.clear()
            inbox.takeovers.clear()
          }
          // Two event shapes exist, one without a sessionID.
          const sessionID = ev?.data?.sessionID ?? ev?.properties?.sessionID
          if (!sessionID) continue
          loop.turns += 1
          if (loop.stopped || loop.paused) {
            const reason = loop.stopped ? (loop.stopReason ?? "stopped") : "paused"
            record("continue.skipped", { reason })
            // A pause announces nothing: only `stop`, which is final,
            // ends the run (DL-039).
            if (loop.stopped) {
              await announceStop(ctx, sessionID, reason)
            }
            continue
          }
          try {
            await continueRun(ctx, sessionID)
            record("continue.sent", { continuations: loop.continuations })
          } catch (e) {
            record("continue.failed", { error: String(e) })
            // A continuation that cannot be sent ends the run just as
            // surely as one never attempted, and nothing else will
            // announce it.
            await announceStop(ctx, sessionID, "continue-failed")
          }
        }
      })().catch((e) => record("event.loop.failed", { error: String(e) }))
      record("event.subscribed", {})
    } catch (e) {
      record("event.failed", { error: String(e) })
    }
  },
}
