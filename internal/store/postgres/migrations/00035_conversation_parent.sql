-- The OpenCode id of the conversation that started this one as a
-- subagent; null for a root.
--
-- @decision(DL-110)

-- +goose Up
alter table conversations add column parent_opencode_id text;

-- +goose Down
alter table conversations drop column parent_opencode_id;
