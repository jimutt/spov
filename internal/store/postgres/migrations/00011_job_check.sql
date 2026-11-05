-- The repo's own check command, and gate 1 of a job's verdict.
--
-- It is on the job row rather than passed to the launch because the
-- launch happens later and on another replica: a job is created queued
-- and provisioned by whichever supervisor picks it up, so anything the
-- sandbox needs has to survive the gap.
--
-- Nullable-by-default rather than required: a job with no check has
-- gate 1 unverified, which DL-050 classifies on the other gates rather
-- than treating as a failure.
--
-- @decision(DL-050)

-- +goose Up
alter table jobs add column check_command text not null default '';

-- +goose Down
alter table jobs drop column check_command;
