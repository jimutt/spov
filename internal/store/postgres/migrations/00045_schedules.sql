-- Scheduled job requests, their history, and the schedule and slot a
-- job was created for; a pair is created at most once.
--
-- @decision(DL-191) @decision(DL-192) @decision(DL-193)

-- +goose Up
create table schedules (
    id            text        primary key,
    org_id        text        not null references orgs (id) on delete cascade,
    owner_id      text        not null,
    name          text        not null,
    request       jsonb       not null,
    cron          text        not null,
    zone          text        not null,
    state         text        not null check (state in ('active', 'paused')),
    paused_reason text        not null default '',
    next_run_at   timestamptz not null,
    failures      int         not null default 0,
    created_at    timestamptz not null,
    updated_at    timestamptz not null,
    unique (org_id, owner_id, name)
);
create index schedules_due on schedules (next_run_at) where state = 'active';

create table schedule_fires (
    id          text        primary key,
    org_id      text        not null,
    schedule_id text        not null references schedules (id) on delete cascade,
    slot        timestamptz not null,
    fired_at    timestamptz not null,
    outcome     text        not null check (outcome in ('started', 'skipped', 'failed', 'missed')),
    reason      text        not null default '',
    job_id      text        not null default '',
    missed      int         not null default 0
);
create index schedule_fires_by_schedule on schedule_fires (schedule_id, id desc);

alter table jobs add column schedule_id text, add column scheduled_for timestamptz;
create unique index jobs_schedule_slot on jobs (schedule_id, scheduled_for) where schedule_id is not null;

-- +goose Down
drop index jobs_schedule_slot;
alter table jobs drop column schedule_id, drop column scheduled_for;
drop table schedule_fires;
drop table schedules;
