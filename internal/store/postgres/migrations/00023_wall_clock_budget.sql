-- A job's wall clock is one allowance across the job: clock_used_seconds
-- is the time of its finished running and blocked stretches, and
-- clock_started_at the start of the current one. A user's
-- default_job_duration_seconds is the duration their jobs get when the
-- request names none; 0 is unset.
--
-- Both default to 0, which leaves an existing job's clock and an
-- existing user's default exactly as before.
--
-- @decision(DL-049)

-- +goose Up
alter table jobs add column clock_used_seconds bigint not null default 0
    check (clock_used_seconds >= 0);
-- A row that left running before this kept its clock_started_at; the
-- ledger reads a set one as a stretch in progress.
update jobs set clock_started_at = null where state not in ('running', 'blocked');
alter table users add column default_job_duration_seconds bigint not null default 0
    check (default_job_duration_seconds >= 0);

-- +goose Down
alter table users drop column default_job_duration_seconds;
alter table jobs drop column clock_used_seconds;
