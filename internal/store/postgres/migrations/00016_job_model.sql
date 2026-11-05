-- The model a job was asked to run on, as OpenCode's own
-- provider/model identifier.
--
-- On jobs alone. A session's model rides in its request, which is
-- persisted as a JSON blob by sessionJSON, so a column beside it would
-- be a second copy of a fact the row already holds. The job row earns
-- its own: it is what a redelivery reads when provisioning lands on
-- another replica, what a cost view groups by in SQL, and what has to
-- be true of the run the verdict describes.
--
-- Empty by default, and every existing row stays empty: a job that
-- named no model keeps meaning "whatever OpenCode resolves", which is
-- the behaviour before this column existed.
--
-- @decision(DL-014)

-- +goose Up
alter table jobs add column model text not null default '';

-- +goose Down
alter table jobs drop column model;
