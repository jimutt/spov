-- An org's own shared credentials, sealed like a user's but under their
-- own AAD, one per platform; and the audit trail of admins changing the
-- org's configuration, which never carries a secret.
--
-- @decision(DL-042)

-- +goose Up
create table org_credentials (
    id          text        primary key,
    org_id      text        not null,
    platform    text        not null,
    ciphertext  bytea       not null,
    nonce       bytea       not null,
    key_version integer     not null,
    meta        jsonb       not null default '{}'::jsonb,
    updated_by  text        not null default '',
    created_at  timestamptz not null,
    updated_at  timestamptz not null,
    unique (org_id, platform)
);

create table admin_events (
    id     text        primary key,
    org_id text        not null,
    actor  text        not null,
    action text        not null,
    detail text        not null default '',
    at     timestamptz not null
);
create index admin_events_org_at on admin_events (org_id, at desc, id desc);

-- +goose Down
drop table admin_events;
drop table org_credentials;
