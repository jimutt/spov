-- +goose Up
-- Who may log in. A file reloaded on SIGHUP (DL-007) reaches one
-- replica; a table is read by all of them.
--
-- The e-mail is the key, not a surrogate ID: an entry exists for an
-- address whether or not anyone has ever logged in under it, so there
-- is nothing else to name it by. It is stored lower case, which the
-- store layer enforces, so the primary key is the case-insensitive
-- uniqueness the file's parser gave for free.
--
-- @decision(DL-007)
create table allowlist_entries (
    org_id     text        not null references orgs (id) on delete cascade,
    email      text        not null,
    admin      boolean     not null default false,
    created_at timestamptz not null,
    primary key (org_id, email)
);

-- "Would this change leave the org with no admin?" runs inside the
-- transaction that makes the change, and the seed asks it at every
-- startup. Both want the admins, not a scan of the whole list.
--
-- @decision(DL-007)
create index allowlist_entries_admins
    on allowlist_entries (org_id)
    where admin;

-- +goose Down
drop index allowlist_entries_admins;
drop table allowlist_entries;
