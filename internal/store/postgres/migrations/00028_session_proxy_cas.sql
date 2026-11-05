-- A sandbox's proxy CA, sealed with the credential key, so a recreated
-- sidecar reuses the CA every process in the sandbox already trusts. A
-- table of its own, so no session read carries it.
--
-- @decision(DL-034)

-- +goose Up
create table session_proxy_cas (
    session_id  text        primary key,
    org_id      text        not null,
    ciphertext  bytea       not null,
    nonce       bytea       not null,
    key_version integer     not null,
    created_at  timestamptz not null,
    foreign key (session_id, org_id) references sessions (id, org_id) on delete cascade
);

-- +goose Down
drop table session_proxy_cas;
