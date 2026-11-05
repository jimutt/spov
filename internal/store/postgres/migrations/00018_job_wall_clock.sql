-- A job's wall clock: how long it may run unattended, and when the
-- current unattended stretch began.
--
-- max_duration_seconds is fixed when the job is created. Zero is a job
-- created before this column existed, which has no clock.
-- clock_started_at is stamped whenever the job enters running, so a
-- resume restarts it (store.StampJobTimes).
--
-- @decision(DL-049)

-- +goose Up
alter table jobs add column max_duration_seconds bigint not null default 0;
alter table jobs add column clock_started_at timestamptz;

-- +goose Down
alter table jobs drop column clock_started_at;
alter table jobs drop column max_duration_seconds;
