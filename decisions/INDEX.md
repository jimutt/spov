# Decision Log

| ID | Title | Status | Namespace | Tags |
|----|-------|--------|-----------|------|
| DL-117 | On a phone the composer is one container: the text over a toolbar holding the model chip and a small Send, folded to one row while idle | accepted | web | web, phone, composer |
| DL-116 | The interface calls the figure sandbox cost, shows it only beside the sandbox, and keeps model token cost visibly untracked | accepted | web | web, cost, usage, sessions |
| DL-115 | A session's last-message preview comes from its root conversations, never from a subagent | accepted | server | subagents |
| DL-114 | Only the job's own conversation can be a person's: the supervisor reads takeovers, withdrawals and the verdict's "a person wrote" from it alone | accepted | jobs | subagents |
| DL-113 | A subagent opens in its own read-only view on the session page, with a way back, its siblings and a list in the sidebar | accepted | web | subagents |
| DL-112 | A subagent is one card in its parent's transcript: its call, state and latest action, opened inline, with asks kept in view | accepted | web | subagents |
| DL-111 | The main transcript draws the root conversation only, and a subagent never speaks as the reader | accepted | web | subagents |
| DL-110 | A conversation records its parent, read from OpenCode when the recorder first sees it, and the wire says which conversations are subagents | accepted | server | subagents |
| DL-109 | An unnamed send steers into the session's oldest conversation | accepted | server | steering, conversations, subagent |
| DL-108 | The workspace guide says that a refused forge command is not an expired token | accepted | opencode | gh-pat-reads, workspace, credentials |
| DL-107 | A GitHub PAT also gets its credential on read-only Actions, releases and search | accepted | sandbox | gh-pat-reads, credentials, proxy |
| DL-106 | On a phone the header is one bar, a session is known by its repository, and the session page keeps its title and composer on screen | accepted | web | web, mobile, layout, composer, jobs |
| DL-105 | Deployment guides orient around the prebuilt release image, with a shipped server user file and a single-operator path | accepted | server | deployment, hosted, docs |
| DL-104 | spov resume syncs a synced session back on exit, and keeps it running outside its checkout | accepted | cli | sync, resume |
| DL-103 | spov jobs wait blocks until a job finishes and exits by its outcome | accepted | cli | sync-jobs, jobs, cli, orchestration |
| DL-102 | A sync job's state is exported over the API, and applied or released by the CLI | proposed | jobs | sync-jobs, jobs, sync, api |
| DL-101 | A sync job is stopped and judged on its live sandbox, finishes, and holds the sandbox for a bounded time | proposed | jobs | sync-jobs, jobs, sync, gates |
| DL-100 | A sync job starts from the local checkout and leaves its work in the repository | proposed | jobs | sync-jobs, jobs, sync |
| DL-099 | The README is for a first-time user; building from source lives in docs/development.md | accepted | cli | docs, onboarding, distribution |
| DL-098 | A transcript resume of a synced session is seeded from a bundle the CLI sends, else from the default branch | accepted | server | sync, resume |
| DL-097 | A user environment variable may not set what spov itself sets or relies on, and is bounded | accepted | sandbox | sandbox-env, sandbox, security |
| DL-096 | A session takes non-secret environment variables from sandbox.env and --env | accepted | server | sandbox-env, config, sandbox |
| DL-095 | A plain spov fly in a git checkout launches that repository, synced | accepted | cli | sync, fly, onboarding, config |
| DL-093 | A local server prunes a stopped session's history 30 days after it stopped, configurable, and never while its snapshot is kept | accepted | server | local-history, retention, config |
| DL-092 | The local journal is a directory per session: session.json rewritten whole, and transcript.jsonl appended record by record | accepted | server | local-history, store, format |
| DL-091 | A local server keeps its sessions in a journal on disk, with the memory store as its engine | accepted | server | local-history, store, local-mode, snapshot-resume |
| DL-090 | Resume is offered on a stopped session's page, in its composer, by spov resume and by spov attach, worded for what will come back | accepted | web | snapshot-resume, web, cli, composer, attach |
| DL-089 | Without a snapshot, a resume clones as at launch and imports each conversation from the store | accepted | server | snapshot-resume, recorder, opencode, store |
| DL-088 | A resume boots from the newest run's exit snapshot, waiting for one still being taken, and the entrypoint takes up the restored workspace | accepted | opencode | snapshot-resume, entrypoint, sandbox |
| DL-087 | Resume reopens a stopped interactive session in a new sandbox, with its settings as launched and only the timeout to change | accepted | server | snapshot-resume, sessions, lifecycle, credentials |
| DL-086 | Snapshot retention, a per-user cap and an off switch are deployment config, and a daily pass prunes past them | accepted | server | snapshot-resume, config, images, cost |
| DL-085 | Each sandbox of a session is a run, and its exit snapshot is collected after it ends | accepted | server | snapshot-resume, store, reconciler, cost |
| DL-084 | An interactive sandbox is created with an exit snapshot when the proxy holds its secrets | accepted | sandbox | snapshot-resume, sandbox, modal, lifecycle |
| DL-083 | A local server can use the gh CLI's own login, opted into once and read again at each launch, and never on a hosted server | accepted | cli | distribution, credentials, gh, local |
| DL-082 | Each release publishes a signed multi-architecture server image to ghcr.io, and the deployment examples pin it by digest | accepted | server | distribution, deployment, image, supply-chain |
| DL-081 | A Homebrew tap carries a cask that goreleaser writes on each published release | accepted | cli | distribution, homebrew |
| DL-080 | install.sh is the headline install: a release asset that verifies before it unpacks, installs to ~/.local/bin, and can fetch the pinned OpenCode | accepted | cli | distribution, install, security |
| DL-079 | A release is built by goreleaser from a tag, its checksums signed keylessly and its archives attested, and it refuses a binary without the web app or the proxy | accepted | server | distribution, release, security, supply-chain |
| DL-078 | A forge token also reads its forge's package registry, for the session's own namespaces | accepted | git | routed-credentials, git, credentials, proxy, packages |
| DL-077 | A server without the proxy refuses to attach a routed credential, and the session page names each one with its hosts | accepted | server | routed-credentials, credentials, security, web |
| DL-076 | Presets for npm, Maven, NuGet and PyPI fill in a routed credential's scheme and write the client's config, and Java trusts the sandbox's CA | accepted | sandbox | routed-credentials, sandbox, presets, toolchain |
| DL-075 | Every session gets the routed credentials it can by default, narrowed by the credential, the project file or the launch, with a default of its own for jobs | accepted | server | routed-credentials, credentials, config, jobs |
| DL-074 | An admin defines the organisation's routed credentials, sealed under the organisation's AAD and audited | accepted | server | routed-credentials, credentials, admin, security |
| DL-073 | A user stores a routed credential with spov credentials add, sealed with its route when hosted and in the keyring with an index file locally | accepted | server | routed-credentials, credentials, security, cli |
| DL-072 | A routed credential is a secret plus a route, and the proxy sidecar sets it only on the route's hosts and paths | accepted | sandbox | routed-credentials, sandbox, proxy, credentials, security |
| DL-071 | A long session costs no scan in the memory store, and its transcript and log are gzipped | accepted | server | server, store, performance, compression |
| DL-070 | The transcript draws a window of its newest 200 blocks, and the tail latch releases only when the reader scrolls up | accepted | web | web, transcript, performance, tail |
| DL-069 | The recorder reads a conversation's messages page by page, newest first, until a page changes nothing in the store | accepted | server | server, recorder |
| DL-068 | A launch carries the user's extra egress domains, bounded, beside the server's | accepted | sandbox | sandbox, egress, config |
| DL-067 | A bad Modal environment on a by-name sandbox lookup is an error, not a missing sandbox | accepted | sandbox | sandbox, modal |
| DL-066 | A synced launch whose checkout matches several platforms is an error | accepted | cli | cli, sync, platforms |
| DL-065 | The connect-token mint limit is exact across replicas | accepted | server | server, connect, postgres |
| DL-064 | A local server listens only on loopback, whatever --listen says | accepted | server | server, local, security |
| DL-063 | Agent-written Markdown is rendered through one sanitised renderer, and a report may show its own collected images | accepted | web | web, markdown, security |
| DL-062 | Settings is a page of independent sections that seed from the profile and clear a setting by sending an empty value | accepted | web | web, settings |
| DL-061 | Jobs and sessions can be started from the browser, through the CLI's own endpoints and with guards against spending twice | accepted | web | web, forms, jobs |
| DL-060 | The sessions and jobs pages are tables of state, show both limits as the server counts them, mark a partial success, and list fifty rows at a time | accepted | web | web, listings, jobs |
| DL-059 | The composer sends for the agent's next step, keeps Interrupt and Resume outside its form, and echoes a message by its id | accepted | web | web, composer, steering |
| DL-058 | The transcript is one timeline of stored messages and events, read once and resumed by cursor, drawing work rather than plumbing | accepted | web | web, transcript, live-read |
| DL-057 | A session's page shows its job in the sidebar, follows the tail as a latch, and offers Cancel and End behind a second press | accepted | web | web, session-page, jobs |
| DL-056 | The web app's design: bundled Atkinson faces, one job per colour at AA in both themes, a state as mark, colour and word, and plain copy | accepted | web | web, design, accessibility |
| DL-055 | The web app is Astro with Svelte 5 islands, embedded in the binary, and reads the API through one data layer | accepted | web | web, stack, embedding |
| DL-054 | Steered means a held loop, and Resume, or an answered permission on a blocked job, releases it | accepted | jobs | jobs, steering, resume |
| DL-053 | Interrupting a job's turn is a sandbox call plus a marker, reads a pending message at once, and holds the loop | accepted | jobs | jobs, steering, interrupt |
| DL-052 | A person's message takes a job over when it is delivered, unless it says to continue, and the plugin holds its loop | accepted | jobs | jobs, steering, plugin |
| DL-051 | A job's deliverables are recorded pull requests and collected reports, checked by spov and stored apart from the job row | accepted | jobs | jobs, deliverables, reports, pull-requests |
| DL-050 | A finished job is judged on three gates, the check, the deliverable and conduct, and never on its own claim | accepted | jobs | jobs, verdict, gates |
| DL-049 | A job's unattended time is one wall-clock allowance, and reaching it ends the run and judges what exists | accepted | jobs | jobs, wall-clock, budgets |
| DL-048 | A run ends on a signal the plugin sends, the completion tool, a stop marker or bounded failures, never on silence | accepted | jobs | jobs, completion, plugin |
| DL-047 | One model pass in the control plane prepares a job's goal, may propose a check, and never fails the job | accepted | jobs | jobs, prep, llm |
| DL-046 | The supervisor is a River worker in internal/jobs that learns everything from the store, never from the sandbox | accepted | jobs | jobs, supervisor, river |
| DL-045 | A job is a row with its own session and quota, carried through a state machine that only the supervisor writes | accepted | jobs | jobs, state-machine |
| DL-044 | A git author is resolved at launch and set in the sandbox, and a missing one only warns | accepted | git | git, identity |
| DL-043 | A sandbox's git credential reaches the session's repositories or, by choice, all of them, and is checked before launch and named on the page | accepted | git | git, credentials, security |
| DL-042 | A launch resolves each platform to a personal or shared credential tier, records it, and every later step uses only that tier | accepted | git | git, credentials, github-app, tiers |
| DL-041 | Four forge kinds sit behind one platform interface, with namespace paths, credential checks and a tested matrix | accepted | git | git, forges, github, forgejo, gitlab |
| DL-040 | A job's permissions are an in-sandbox allowlist per kind: a guardrail on the agent's tool calls, never a boundary | accepted | opencode | opencode, plugin, permissions, security |
| DL-039 | A job's run is continued by one import-free plugin that re-asserts the goal on every request and orients the agent | accepted | opencode | opencode, plugin, jobs |
| DL-038 | Every sandbox gets an OpenCode config file: an interactive one may work across /workspace, and a job's carries no permission keys | accepted | opencode | opencode, config, permissions |
| DL-037 | Every session lands at /workspace, with a generated AGENTS.md, an out directory and a scratchpad that no clone may take | accepted | opencode | opencode, workspace, landing, scratchpad |
| DL-036 | The entrypoint clones safely and in parallel, then starts OpenCode in the foreground with no credential in argv | accepted | opencode | opencode, entrypoint, sandbox |
| DL-035 | One exact OpenCode pin, published by the server, gates the sandbox exactly and the local client by version part, and a bump is checked against the pinned build's API | accepted | opencode | opencode, version, contract |
| DL-034 | Forge tokens and provider keys live in a proxy sidecar that sets them only on allowed hosts and endpoints | accepted | sandbox | sandbox, security, proxy, credentials |
| DL-033 | Add-ons are one curated list resolved to exact versions, installed from pinned sources, started before OpenCode and described to the agent | accepted | sandbox | sandbox, addons, toolchain |
| DL-032 | Sandboxes run a pinned, layered base image with the default toolchain and the forge CLIs, built by an operator command | accepted | sandbox | sandbox, image, toolchain |
| DL-031 | Egress is a required policy reserved open at create, chosen from presets, and it is never what contains a job | accepted | sandbox | sandbox, egress, security |
| DL-030 | A sandbox exposes no ports: the server mints connect tokens, a loopback proxy holds every credential, and the server reuses its own connections | accepted | sandbox | sandbox, security, proxy |
| DL-029 | Modal is reached only through a small Provider interface, and every sandbox is tagged, bounded and ephemeral | accepted | sandbox | sandbox, modal, lifecycle |
| DL-028 | One repository picker merges every platform, as spov's own bubbletea model with fuzzy ranking over the visible row | accepted | cli | cli, tui, picker |
| DL-027 | A session can start from the local branch and sync its work back, opt-in and for one repository | accepted | cli | cli, sync, git |
| DL-026 | Configuration is layered from the user file, the project file and the environment, and a project file cannot say where credentials go | accepted | cli | config |
| DL-025 | spov attach waits until a session can be joined, joins its conversation, and hands over the terminal or the browser | accepted | cli | cli, attach, tui |
| DL-024 | spov fly shows a terminal-only launch view of the steps it can observe, and hands the terminal straight to OpenCode | accepted | cli | cli, tui, launch |
| DL-023 | The command tree: session verbs at the top, a jobs group, one meaning per verb, --json for scripts, and completion from a running server | accepted | cli | cli, commands |
| DL-022 | Each combined scenario is validated in layers, on one harness built from spov serve's own wiring | accepted | server | testing, scenarios |
| DL-021 | The server re-mints a session's GitHub App token while the session is in use, and notes a failed renewal once | accepted | server | git, credentials, renewal |
| DL-020 | A pending ask is read from stored events and answered through the server, allow once or reject, on a card in the transcript | accepted | server | asks, permissions, steering |
| DL-019 | A message to a session goes through the server as a write, and is read at the agent's next step by default | accepted | server | steering, messages |
| DL-018 | Every read path answers from the store, and nothing a reader does keeps a sandbox alive | accepted | server | live-read, sse, idle-clock |
| DL-017 | One leader-run recorder writes each running session's events to the store, and marks a gap only where the durable sequence skips | accepted | server | recorder, store, events |
| DL-016 | The server records the images it publishes, builds a missing add-on variant on demand, and prunes what goes unused | accepted | server | images, sandbox-toolchain |
| DL-015 | The server runs the OpenCode Console sign-in, refreshes the grant only at launch, and the proxy sets its token only on listed paths | accepted | server | opencode-console, credentials |
| DL-014 | A session's model is one field, resolved from the request, the user's default or OpenCode's own, and switchable while it runs | accepted | server | models, sessions |
| DL-013 | The model list is models.dev's over a static provider literal, and a model is checked before a sandbox is bought | accepted | server | models, providers |
| DL-012 | Listings are newest first and paged by an ID cursor, and every live entry is always shown | accepted | server | api, listing, cli |
| DL-011 | Metrics are OpenTelemetry instruments with a closed catalogue, exported only when configured, and shared state is measured once | accepted | server | metrics, telemetry |
| DL-010 | A hosted server is one static binary behind one proxy, configured from the environment, with its own Modal credential | accepted | server | deployment, hosted |
| DL-009 | Per-user quotas are enforced before anything billable, with sessions and jobs counted apart under advisory locks | accepted | server | quotas, cost |
| DL-008 | Secrets live in the OS keyring locally and sealed in the database when hosted, and the CLI adds them with spov credentials | accepted | server | credentials, security, cli |
| DL-007 | Web login is GitHub OAuth gated by a store-backed allowlist, which alone says who is an administrator | accepted | server | auth, login, allowlist |
| DL-006 | One Principal behind pluggable authenticators, the browser guard on every listener, and spov's own device flow issuing hashed tokens | accepted | server | auth, security, device-flow |
| DL-005 | The HTTP API is Huma on the stdlib mux with the OpenAPI document as its contract, and the web client is generated from it | accepted | server | api, openapi, web |
| DL-004 | One leader-locked reconciler lists spov's sandboxes by tag, adopts, sweeps and reaps, and leaves a job's sandbox to its job | accepted | server | reconciler, control-plane |
| DL-003 | One Store interface with memory and Postgres implementations, and no request depends on the replica that launched | accepted | server | store, postgres, replicas |
| DL-002 | The CLI always speaks HTTP to a spov server, and a local server is a spawned child that exits when idle | accepted | server | architecture, cli, local-server |
| DL-001 | spov is one Go binary holding the CLI and the control plane, with Modal as the sandbox provider | accepted | server | architecture, go, stack |
