-- The provider/model reference a user's launches run on when the
-- request names none.
--
-- On the profile rather than in a settings table: the row already
-- holds the git identity and a quota override, and a second mechanism
-- for the same kind of fact would be a second thing to read, migrate
-- and keep in step.
--
-- Empty by default, and every existing row stays empty: a user who has
-- set nothing keeps meaning "whatever OpenCode resolves", which is the
-- behaviour before this column existed.
--
-- @decision(DL-014)

-- +goose Up
alter table users add column default_model text not null default '';

-- +goose Down
alter table users drop column default_model;
