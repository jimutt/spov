-- OpenCode's Session.Info for each conversation, whole, as the
-- sandbox's listing last gave it. A transcript resume imports it.
--
-- @decision(DL-089)

-- +goose Up
alter table conversations add column info bytea;

-- +goose Down
alter table conversations drop column info;
