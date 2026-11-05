-- +goose Up
-- Every table carries org_id; ids are ULIDs as text. Nothing here needs a
-- feature newer than PostgreSQL 16.

create table orgs (
    id          text primary key,
    name        text not null,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

-- Multi-org is rows, not a migration.
insert into orgs (id, name) values ('default', 'default');

-- The bearer-token principal of DL-006 carries OrgID "local", and it is
-- the only authenticator until DL-007 adds login. Without this row every
-- session insert fails its foreign key, so a hosted server running on
-- the token authenticator can create nothing.
insert into orgs (id, name) values ('local', 'local');

create table users (
    id            text primary key,
    org_id        text not null references orgs (id) on delete cascade,
    email         text not null,
    name          text not null default '',
    github_login  text not null default '',
    admin         boolean not null default false,
    git_name      text not null default '',
    git_email     text not null default '',
    last_login_at timestamptz,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);

-- E-mail identifies a person, so it is unique per org regardless of case.
create unique index users_org_email on users (org_id, lower(email));

create table sessions (
    id         text primary key,
    org_id     text not null references orgs (id) on delete cascade,
    owner_id   text not null,
    name       text not null,
    state      text not null check (state in ('starting', 'running', 'stopped', 'failed')),
    sandbox_id text not null default '',
    request    jsonb not null default '{}'::jsonb,
    clones     jsonb not null default '[]'::jsonb,
    landing    text not null default '',
    error      text not null default '',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    stopped_at timestamptz
);

-- "A name belongs to at most one live session per org", structurally.
create unique index sessions_live_name
    on sessions (org_id, name)
    where state in ('starting', 'running');

-- The reconciler's two queries.
create index sessions_sandbox_id on sessions (sandbox_id) where sandbox_id <> '';
create index sessions_live on sessions (state) where state in ('starting', 'running');
create index sessions_org_owner on sessions (org_id, owner_id);

create table mints (
    id         text primary key,
    org_id     text not null references orgs (id) on delete cascade,
    session_id text not null references sessions (id) on delete cascade,
    user_id    text not null default '',
    port       integer not null default 0,
    expires_at timestamptz,
    created_at timestamptz not null default now()
);

-- The rolling-minute rate limit counts on this.
create index mints_session_created on mints (session_id, created_at);

create table api_tokens (
    id           text primary key,
    org_id       text not null references orgs (id) on delete cascade,
    user_id      text not null,
    token_hash   bytea not null unique,
    name         text not null default '',
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now()
);

create index api_tokens_user on api_tokens (org_id, user_id);

create table device_codes (
    id               text primary key,
    org_id           text not null references orgs (id) on delete cascade,
    device_code_hash bytea not null unique,
    user_code        text not null unique,
    user_id          text not null default '',
    status           text not null check (status in ('pending', 'approved', 'denied')),
    name             text not null default '',
    expires_at       timestamptz not null,
    last_polled_at   timestamptz,
    interval_s       integer not null default 5,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now()
);

create index device_codes_expires on device_codes (expires_at);

create table user_credentials (
    id          text primary key,
    org_id      text not null references orgs (id) on delete cascade,
    user_id     text not null,
    platform    text not null,
    ciphertext  bytea not null,
    nonce       bytea not null,
    key_version integer not null default 1,
    verified_as text not null default '',
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    unique (org_id, user_id, platform)
);

-- +goose Down
drop table user_credentials;
drop table device_codes;
drop table api_tokens;
drop table mints;
drop table sessions;
drop table users;
drop table orgs;
