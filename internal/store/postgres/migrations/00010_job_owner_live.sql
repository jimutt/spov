-- The job concurrency quota counts a user's unfinished jobs inside the
-- create transaction, so this is on a request path and wants its own
-- index rather than a scan of the org's jobs.
--
-- The predicate matches the one on jobs_live, but the columns do not:
-- jobs_live serves the supervisor's cross-org restart query and leads
-- with state, which is the wrong order for a count scoped to one owner.
--
-- @decision(DL-045)

-- +goose Up
create index jobs_owner_live
    on jobs (org_id, owner_id)
    where state not in ('succeeded', 'failed', 'cancelled');

-- +goose Down
drop index jobs_owner_live;
