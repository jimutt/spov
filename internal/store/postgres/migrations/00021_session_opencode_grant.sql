-- Which generation of its owner's OpenCode Console grant a session's
-- sidecar holds. The renewal pass writes a newer token into a session
-- that is behind. Zero means the session holds none.
--
-- @decision(DL-015)

-- +goose Up
alter table sessions add column opencode_grant integer not null default 0;

-- +goose Down
alter table sessions drop column opencode_grant;
