-- +goose Up
-- What a sandbox was given for each platform (DL-043), and when its
-- renewable git token was minted and lapses (DL-021). The token itself is
-- never stored.
--
-- @decision(DL-021) @decision(DL-043)
alter table sessions add column credentials jsonb not null default '[]';
alter table sessions add column git_token_minted_at timestamptz;
alter table sessions add column git_token_expires_at timestamptz;

-- +goose Down
alter table sessions drop column git_token_expires_at;
alter table sessions drop column git_token_minted_at;
alter table sessions drop column credentials;
