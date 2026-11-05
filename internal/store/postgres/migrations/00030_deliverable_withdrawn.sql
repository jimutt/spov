-- A pull request the agent withdrew after a person wrote to the job,
-- which gate 2 leaves out of the set.
--
-- @decision(DL-051)

-- +goose Up
alter table job_deliverables drop constraint job_deliverables_status_check;
alter table job_deliverables add constraint job_deliverables_status_check
    check (status in ('verified', 'missing', 'unverified', 'over_limit', 'withdrawn'));

-- +goose Down
delete from job_deliverables where status = 'withdrawn';
alter table job_deliverables drop constraint job_deliverables_status_check;
alter table job_deliverables add constraint job_deliverables_status_check
    check (status in ('verified', 'missing', 'unverified', 'over_limit'));
