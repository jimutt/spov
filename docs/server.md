# The server and the dashboard

`spov serve` is spov's control plane: it holds the credentials, creates the
sandboxes and supervises the jobs. The `spov` CLI and the web dashboard are both
clients of it. You rarely start it yourself.

## Local or with a database

| | **Local** | **With a server and a database** |
|---|---|---|
| Setup | None: the CLI starts its own server | `spov serve` with Postgres; see [`deploy/README.md`](../deploy/README.md) |
| Interactive sessions (`spov fly`) | Yes | Yes |
| Autonomous jobs (`spov jobs create`) | No | Yes |
| Web dashboard | No | Yes, with GitHub sign-in |
| Credentials | On your machine (keyring or a 0600 file) | Per user, encrypted on the server |
| Users | You | Several, with an allowlist and quotas |

The server with a database can run on your own machine too. It is what the
dashboard and jobs need, not a remote host.

## The local server

By default there is nothing to set up. The CLI starts a server for itself the
first time it needs one (`spov serve --local`, on an ephemeral loopback port),
finds it again through `~/.local/state/spov/server.json`, and the server exits
after a minute with nothing to do (end the `spov serve --local` process to
restart it sooner, say after storing a model key). Its log is `~/.local/state/spov/server.log`,
which is where to look if a launch fails early. After an upgrade the old
server is stopped and replaced, so two builds never share one.

The local server keeps its sessions' history on disk, so a stopped session can
be resumed after a restart ([local-history.md](local-history.md)).

## A hosted server

A server is used instead of the local one when it is named: the `--server`
flag, then `SPOV_SERVER`, then `server.url` in config. Sign in with
`spov login`: spov prints a code to enter in a browser and stores the token
that comes back. `spov whoami` shows who the server sees, and `spov logout`
revokes the token.

Running a server, with its database, browser login, allowlist, quotas,
credentials, health checks and the published image, is covered in
[`deploy/README.md`](../deploy/README.md).

## The web dashboard

A server with a database also serves a dashboard, signed in to with GitHub.
[`deploy/README.md`](../deploy/README.md) covers the OAuth app and the
allowlist.

- **Sessions**: every session, live, with its state, its job if it has one,
  and its age.
- **Jobs**: every job, filtered by All, Active, Needs you (blocked or steered)
  and Finished, with each job's outcome.
- **Start a session** and **Start a job**: from a form each, with a picker of
  your repositories. The job form makes the same job as `spov jobs create`.
- **A session's page** is also its job's page:
  - the live transcript;
  - the job's state, time limit, verdict gate by gate, and its pull request or
    report;
  - a message box to steer it, a model switch, and answers to the agent's
    permission questions;
  - **Interrupt** to stop the current turn, **Resume** to hand a paused job
    back or reopen a stopped session, **Cancel job** and **End session**. The
    last two act only on a second press;
  - what credentials and environment variables the sandbox was given.
- **Usage**: sessions, sandbox hours and estimated sandbox cost this month.
  Model token cost is marked as not yet tracked.
- **Tokens**: your CLI tokens, which you can revoke.
- **Settings**: your stored credentials and OpenCode connection, default
  model, git access, job time limit and git identity. An admin also stores the
  organisation's GitHub App here (see [credentials.md](credentials.md#github)).

The browser never talks to a sandbox directly. The page shows what the server
has recorded, and an open tab does not keep a sandbox alive.
