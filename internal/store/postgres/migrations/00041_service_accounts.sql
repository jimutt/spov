-- A user row is a person, who signs in, or a service account, which an
-- admin creates and which never signs in. A service account is never
-- bound to a GitHub account, and disabled_at is set when it is deleted;
-- the row stays, so its name stays taken and its history is kept.
--
-- @decision(DL-140)

-- +goose Up
alter table users add column kind text not null default 'person';
alter table users add column disabled_at timestamptz;
alter table users add constraint users_kind check (kind in ('person', 'service'));
alter table users add constraint users_service_unbound check (kind = 'person' or github_id is null);
alter table users add constraint users_disabled_service check (disabled_at is null or kind = 'service');
-- The admin who issued a service account's token; null on a token the
-- device flow issued. @decision(DL-141)
alter table api_tokens add column issued_by text;

-- +goose Down
alter table api_tokens drop column issued_by;
alter table users drop constraint users_disabled_service;
alter table users drop constraint users_service_unbound;
alter table users drop constraint users_kind;
alter table users drop column disabled_at;
alter table users drop column kind;
