-- Each sandbox a session had, numbered from 1, with its exit snapshot.
-- Live sessions get run 1 from what the session row holds; a session
-- that had stopped before this has none and is not resumable.
--
-- @decision(DL-085)

-- +goose Up
create table session_runs (
    session_id      text        not null references sessions (id) on delete cascade,
    org_id          text        not null,
    owner_id        text        not null,
    n               integer     not null check (n > 0),
    sandbox_id      text        not null default '',
    booted          text        not null default '',
    booted_image_id text        not null default '',
    opencode        text        not null default '',
    origin          text        not null check (origin in ('launch', 'snapshot', 'transcript')),
    started_at      timestamptz not null,
    running_at      timestamptz,
    ended_at        timestamptz,
    snapshot        text        not null default '',
    snapshot_state  text        not null check (snapshot_state in ('none', 'pending', 'kept', 'failed', 'deleted')),
    snapshot_at     timestamptz,
    first_message   bytea,
    primary key (session_id, n)
);
create index session_runs_ended on session_runs (snapshot_state, ended_at) where ended_at is not null;

insert into session_runs (session_id, org_id, owner_id, n, sandbox_id, origin, started_at, running_at, snapshot_state)
select id, org_id, owner_id, 1, sandbox_id, 'launch', created_at,
       case when state = 'running' then created_at end, 'none'
from sessions where state in ('starting', 'running');

-- +goose Down
drop table session_runs;
