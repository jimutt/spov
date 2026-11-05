-- +goose Up
-- The browser session store (scs/pgxstore). The table name is not the
-- library's default "sessions", which is already taken by spov's own
-- sessions; the manager is configured with this name.
--
-- The token is opaque and nothing here is signed: revoking a login is a
-- delete from this table.
create table web_sessions (
    token  text primary key,
    data   bytea not null,
    expiry timestamptz not null
);

-- scs's cleanup sweep and every expiry check read this.
create index web_sessions_expiry on web_sessions (expiry);

-- +goose Down
drop table web_sessions;
