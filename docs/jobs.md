# Autonomous jobs

A job is a goal you hand off. spov launches a sandbox, gives OpenCode the goal,
keeps it working until it says the work is done, and then checks the result
for itself before calling the job a success. You can watch, steer or take over
at any point, from the terminal or the dashboard, and the job outlives your
terminal.

Jobs need a server with a database: `spov serve` with Postgres, on your own
machine or a host. The quickest is the Compose file
([`deploy/README.md`](../deploy/README.md#compose)); then point the CLI at it
and sign in ([The server](server.md#a-hosted-server)). The local server the CLI
starts for itself runs sessions only.

```sh
spov jobs create --repo acme/app --check './app/scripts/check.sh' \
  "Fix the flaky retry test in internal/queue and open a pull request"

spov jobs create --kind report --repo acme/app --goal-file audit.md

# inside a checkout, with nothing uncommitted: the work comes back here
spov jobs create --kind sync --wait "Port the CLI tests to the new fixture"
spov jobs apply <id>
```

## Creating one

| Flag | Effect |
|---|---|
| `--kind pr\|report\|sync` | What the job delivers: pull requests (merge requests on GitLab; the default), recorded one by one in the repositories you selected; Markdown reports, one file each in `/workspace/out/`; or the repository's state, taken back into the checkout you created it in ([Sync jobs](#sync-jobs)) |
| `--check <command>` | A command that verifies the work, run at `/workspace` (in the repository for a sync job). Its exit status is one of the job's gates. |
| `--max-duration 45m` | How long the job may run unattended. When unset: `sandbox.job_max_duration` in your config, then your default from Settings, then 1 h. Never more than your limit. |
| `--model provider/model` | Model to run on; your account's default when unset |
| `--repo`, `--git-access`, `--env`, `--credentials`, `--no-credentials` | As for sessions. A job takes its git access from `sandbox.job_git_access` and your Settings, and its credentials from `sandbox.job_credentials`, never from the interactive defaults. |
| `--landing <dir>` | Start in one repository's directory rather than `/workspace` |
| `--goal-file <file>` | Read the goal from a file |
| `--wait`, `--timeout <d>` | Wait for the job after creating it, as `spov jobs wait` does |
| `--hold <d>` | A sync job's hold: how long its sandbox is kept after it finishes, for `spov jobs apply`. When unset: `sandbox.job_sync_hold`, then 30 m. |

## Following and steering it

| Command | What it does |
|---|---|
| `spov jobs` (`list`) | Jobs newest first, five a page (`-n`, `--page`), plus any unfinished job older than that |
| `spov jobs show <id>` | One job in full, with what it delivered |
| `spov jobs report <id> [<path>]` | Print a report; `--save <dir>` writes it with its images. Printed raw to a pipe or a file; to a terminal its control characters are shown as escapes |
| `spov attach <session>` | Join the job's conversation, waiting while it is queued and starting |
| `spov jobs wait <id>` | Block until the job finishes, then exit by how it ended ([From a script](#from-a-script)) |
| `spov jobs interrupt <id>` | Stop its current turn and take it over |
| `spov jobs resume <id>` | Hand a steered or blocked job back to its loop |
| `spov jobs cancel <id>` | End it |
| `spov jobs apply <id>` | Land a sync job's work in this checkout ([Sync jobs](#sync-jobs)) |
| `spov jobs release <id>` | Discard a held sync job's work and end its sandbox |

## From a script

`spov jobs wait <id>` blocks until the job is `succeeded`, `failed` or
`cancelled` and prints it as `jobs show` does (`--json` prints the job view and
nothing else on stdout). A job that is `blocked` or `steered` is not finished,
so the wait goes on through it. It reads the job from the server every 5 s,
which never touches the sandbox. When stderr is a terminal it writes a line
each time the state changes.

| Exit status | Meaning |
|---|---|
| 0 | succeeded |
| 3 | succeeded, partly |
| 4 | failed |
| 5 | cancelled |
| 6 | `--timeout` ran out; the job goes on |
| 1 | the command itself could not finish: a bad flag, sign-in, an unknown ID, or the server still unreachable after five retries over about two minutes |
| 130 | interrupted with Ctrl-C; the job goes on |

The job is printed before a non-zero outcome status. A timeout or Ctrl-C ends
only the wait, never the job.

```sh
id=$(spov jobs create --json --repo acme/app "Fix the flaky test" | jq -r .id)
spov jobs wait "$id" --json --timeout 2h > job.json
case $? in
  0) echo "done" ;;
  3) echo "done, partly" ;;
  6) echo "still running" ;;
  *) echo "did not succeed" ;;
esac
```

`spov jobs create --wait` does both in one command. It prints the created job
first, so the IDs are known before the job runs, then waits with the same
`--timeout` and exit status. With `--json` that is two JSON documents, one per
line: the created job, then the final one.

## How a job runs

It moves through `queued`, `provisioning`, `running` and `delivering` to
`succeeded` or `failed` (or `cancelled`). Before provisioning, the server can
run one short model call that assesses the goal and may propose a check; a
check you gave always wins. Two states wait on a person:

- `blocked`: the agent is waiting on a permission question it cannot answer
  itself.
- `steered`: someone sent the job a message or interrupted it. The agent reads
  the message and the job waits until someone resumes it.

**Its time limit.** A job has one time allowance. It counts while the job runs
or is blocked, pauses while someone steers it, and a resume continues with
what is left. No number of turns ends a job. A run that keeps repeating a call
its policy denies is stopped as a safeguard. The job's sandbox lives long
enough for its clock, its launch and delivery, and an hour of steering.

## How it is judged

When the agent declares the work done, or the time limit runs out, spov stops
the run and checks three gates:

1. **Check**: the check command exits 0.
2. **Deliverable**: each pull request the agent recorded is open or merged on
   its forge, or each report in `/workspace/out/` was collected. Some but not
   all is a partial success; none verified is a failure, whatever the check
   said. If you ask the agent to drop a pull request it recorded, it can
   withdraw it, and a withdrawn one is left out of the count; a withdrawal
   counts only after you have written to the job.
3. **Conduct**: the agent actually did something, with no permission request
   left unanswered.

The job succeeds only if the gates allow it, and a partial success says what
is missing or what the agent left unfinished. The agent saying it is finished
is never evidence. Something spov could not check is `unverified`, not
`failed`.

## Sync jobs

A sync job works on the checkout you create it in, as a synced `spov fly`
does, and hands back the repository rather than a pull request. Run
`spov jobs create --kind sync` inside the checkout, on a branch, with nothing
uncommitted. The sandbox starts from your branch, unpushed commits included;
a checkout whose remote is on no configured platform, or that has none, is
sent whole, and its sandbox cannot push. What is sent is at most 16 MiB, so
push a long unpushed history first.

The job lands in the repository's directory, so `--check ./scripts/check.sh`
runs the repository's own script. When it ends, spov stops the agent and reads
the repository: commits since your branch, or uncommitted changes, are the
deliverable. Nothing changed is a failure, and so is a change over the 32 MiB
a sync can bring back.

The job then finishes, and its sandbox is **held** so you can take the work:

```sh
spov jobs apply <id>     # lands it here, then ends the sandbox (--keep to keep it)
spov jobs release <id>   # discards it and ends the sandbox
```

`apply` lands the work in place when the checkout is still on the branch and
commit the job started from, else on a branch named for the job. It then lists
what changed, as a synced `spov fly` does ([Working from a
checkout](sessions.md#working-from-a-checkout)). It also
works on a job still running, steered or blocked, to take its work so far.
A held job keeps its place in your job limit and bills until it is applied,
released or its hold runs out; `spov jobs`, `spov ls` and the dashboard mark
it `held`. Nobody can attach to it or stop it by accident: `spov stop` and
`spov stop --all` leave it, and name the two commands. Over the API, a client
reads the state with `POST /api/v1/jobs/{id}/sync-export` and ends the hold
with `POST /api/v1/jobs/{id}/release`.

## What a job's agent may do

A job's agent runs under a permission allowlist for its kind: a `report` job,
for instance, may write only to `/workspace/out` and its scratchpad. The
allowlist is a guardrail on an honest run, not a security boundary; what
bounds a misbehaving agent is the credential it holds and the proxy's rules
([The sandbox](sandbox.md#credentials-stay-outside-the-sandbox)).

## Limits

Each user has a limit on concurrent jobs, separate from the one on sessions.
A held sync job counts against it until it is applied or released.
See [`deploy/README.md`](../deploy/README.md) for quotas.
