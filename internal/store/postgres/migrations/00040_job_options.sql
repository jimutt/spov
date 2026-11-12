-- A pr job's pull request policy (empty meaning spov's) and whether the
-- job skips the preparation pass. Both are fixed at create.
--
-- @decision(DL-137) @decision(DL-138)

-- +goose Up
alter table jobs add column pr_policy text not null default ''
    check (pr_policy in ('', 'spov', 'goal'));
alter table jobs add column skip_prep boolean not null default false;

-- +goose Down
alter table jobs drop column skip_prep;
alter table jobs drop column pr_policy;
