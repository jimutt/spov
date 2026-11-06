-- OpenCode's Session.Info for each conversation, whole, as the
-- sandbox's listing last gave it. A transcript resume imports it.
--
-- @decision(DL-089)

-- A transcript resume's import restarts a conversation's durable log, so
-- the recorder maps it onto the stored sequence with these two.
--
-- @decision(DL-089)

-- +goose Up
alter table conversations add column info bytea;
alter table conversations add column seq_base bigint not null default 0;
alter table conversations add column seq_after bigint not null default 0;

-- +goose Down
alter table conversations drop column info;
alter table conversations drop column seq_base;
alter table conversations drop column seq_after;
