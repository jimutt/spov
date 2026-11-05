-- What a forge said about a user's token when it was stored: its scopes
-- and when it expires, shown where the credential is listed. Never the
-- token.
--
-- @decision(DL-041)

-- +goose Up
alter table user_credentials
    add column scopes     text[]      not null default '{}',
    add column expires_at timestamptz;

-- +goose Down
alter table user_credentials
    drop column scopes,
    drop column expires_at;
