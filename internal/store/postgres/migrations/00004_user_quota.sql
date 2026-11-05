-- +goose Up
-- A user's own limits, overriding the org defaults field by field. It is
-- jsonb rather than columns because the set of limits is expected to
-- grow, and an override asserts a subset: a null column and a column
-- meaning "no limit" would be indistinguishable.
--
-- @decision(DL-009)
alter table users add column quota jsonb not null default '{}'::jsonb;

-- The concurrency quota counts a user's live sessions inside the create
-- transaction, so this is on a request path and wants its own index
-- rather than a scan of the org's sessions.
--
-- @decision(DL-009)
create index sessions_owner_live
    on sessions (org_id, owner_id)
    where state in ('starting', 'running');

-- +goose Down
drop index sessions_owner_live;
alter table users drop column quota;
