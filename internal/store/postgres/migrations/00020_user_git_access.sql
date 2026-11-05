-- The git access a user's launches get when they name none: one for
-- interactive sessions and one for jobs, "repos", "all" or empty.
--
-- Empty by default, and every existing row stays empty, which means
-- "repos", the behaviour before these columns existed.
--
-- @decision(DL-043)

-- +goose Up
alter table users add column default_git_access text not null default ''
    check (default_git_access in ('', 'repos', 'all'));
alter table users add column default_job_git_access text not null default ''
    check (default_job_git_access in ('', 'repos', 'all'));

-- +goose Down
alter table users drop column default_job_git_access;
alter table users drop column default_git_access;
