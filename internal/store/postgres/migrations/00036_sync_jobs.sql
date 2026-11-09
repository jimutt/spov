-- The sync job kind. A job's seed (the bundle of commits its checkout's
-- remote lacks) waits in job_seeds from create until the launch applies
-- it, and is gone once the job ends. A sync job's deliverable is the
-- repository's state, measured: its head commit, how many commits and
-- changed files, and the export's size. sync_applied_at is when an
-- apply released the held sandbox.
--
-- @decision(DL-100) @decision(DL-101)

-- +goose Up
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check check (kind in ('pr', 'report', 'sync'));
alter table jobs add column sync_applied_at timestamptz;

create table job_seeds (
    job_id     text        primary key references jobs (id) on delete cascade,
    bundle     bytea       not null,
    created_at timestamptz not null
);

alter table job_deliverables drop constraint job_deliverables_kind_check;
alter table job_deliverables add constraint job_deliverables_kind_check
    check (kind in ('pr', 'report', 'sync'));
alter table job_deliverables add column head_commit text not null default '';
alter table job_deliverables add column commits integer not null default 0;
alter table job_deliverables add column files integer not null default 0;

-- +goose Down
alter table job_deliverables drop column files;
alter table job_deliverables drop column commits;
alter table job_deliverables drop column head_commit;
alter table job_deliverables drop constraint job_deliverables_kind_check;
alter table job_deliverables add constraint job_deliverables_kind_check
    check (kind in ('pr', 'report'));
drop table job_seeds;
alter table jobs drop column sync_applied_at;
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check check (kind in ('pr', 'report'));
