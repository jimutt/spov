-- A job's title, from the preparation pass; null when it has none.
--
-- @decision(DL-150)

-- +goose Up
alter table jobs add column title text;

-- +goose Down
alter table jobs drop column title;
