# Decision Log

| ID | Title | Status | Namespace | Tags |
|----|-------|--------|-----------|------|
| DL-069 | The recorder reads a conversation's messages page by page | proposed | server | server, recorder |
| DL-068 | Where a user's extra egress domains are honoured | proposed | sandbox | sandbox, egress, config |
| DL-067 | A bad Modal environment on a by-name sandbox lookup is an error, not a missing sandbox | proposed | sandbox | sandbox, modal |
| DL-066 | A synced launch whose checkout matches several platforms is an error | proposed | cli | cli, sync, platforms |
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
