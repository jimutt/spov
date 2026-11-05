-- +goose Up
-- One piece of supervised work: a goal, the session running it, and
-- where it has got to. The server owns this row and its states; a
-- plugin inside the sandbox owns continuation, and may fail without the
-- job failing.
--
-- The state check is the machine's alphabet, not its transitions. The
-- transitions live in Go (store.CanTransition) because they are read by
-- the supervisor and the job surface and a table there can be asserted
-- against in one test; a check constraint here would be a second copy
-- to keep in step.
--
-- @decision(DL-045)
create table jobs (
    id         text        primary key,
    org_id     text        not null references orgs (id) on delete cascade,
    owner_id   text        not null default '',
    session_id text        not null,
    kind       text        not null check (kind in ('pr', 'report')),
    state      text        not null check (state in (
        'queued', 'provisioning', 'running', 'delivering',
        'succeeded', 'failed', 'blocked', 'steered', 'cancelled'
    )),
    goal       text        not null,
    result     text        not null default '',
    error      text        not null default '',
    attempt    integer     not null default 0,
    created_at timestamptz not null,
    updated_at timestamptz not null,
    started_at timestamptz,
    stopped_at timestamptz,
    -- A session running two loops has two things continuing it, and
    -- neither would know about the other.
    unique (org_id, session_id),
    -- The composite key makes the job agree with its session's org: a
    -- job filed under another org than its session would otherwise be
    -- stored and listed there. The same shape as conversations.
    foreign key (session_id, org_id) references sessions (id, org_id) on delete cascade
);

-- The supervisor's restart query: every unfinished job, across orgs. A
-- restarted replica has no principal to scope by, so this one index
-- serves the only listing that does not filter on org_id.
--
-- @decision(DL-045)
create index jobs_live on jobs (state) where state not in ('succeeded', 'failed', 'cancelled');

-- +goose Down
drop table jobs;
