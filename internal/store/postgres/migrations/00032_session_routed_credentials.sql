-- The routed credentials a launch attached to a session: name, tier and
-- route, never a secret. A recreated sidecar resolves exactly these.
--
-- @decision(DL-075)

-- +goose Up
alter table sessions add column routed_credentials jsonb not null default '[]';

-- +goose Down
alter table sessions drop column routed_credentials;
