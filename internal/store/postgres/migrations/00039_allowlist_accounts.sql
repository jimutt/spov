-- A user is a GitHub account, and an allowlist entry names an address
-- or a GitHub account. A user row from before this has no github_id
-- until its next sign-in binds it; once set, it never changes.
--
-- An entry has an address or an account ID, never both, and each is
-- unique per organisation. The (org_id, email) primary key cannot hold
-- an account entry, so it becomes two partial unique indexes.
--
-- @decision(DL-128)

-- +goose Up
alter table users add column github_id bigint;
create unique index users_org_github_id on users (org_id, github_id) where github_id is not null;

alter table allowlist_entries drop constraint allowlist_entries_pkey;
alter table allowlist_entries alter column email set default '';
alter table allowlist_entries add column github_id bigint;
alter table allowlist_entries add column github_login text not null default '';
alter table allowlist_entries add column github_name text not null default '';
alter table allowlist_entries add constraint allowlist_entries_one_kind
    check ((email = '') = (github_id is not null));
create unique index allowlist_entries_org_email on allowlist_entries (org_id, email) where github_id is null;
create unique index allowlist_entries_org_github_id on allowlist_entries (org_id, github_id)
    where github_id is not null;

-- +goose Down
delete from allowlist_entries where github_id is not null;
drop index allowlist_entries_org_github_id;
drop index allowlist_entries_org_email;
alter table allowlist_entries drop constraint allowlist_entries_one_kind;
alter table allowlist_entries drop column github_name;
alter table allowlist_entries drop column github_login;
alter table allowlist_entries drop column github_id;
alter table allowlist_entries alter column email drop default;
alter table allowlist_entries add primary key (org_id, email);

drop index users_org_github_id;
alter table users drop column github_id;
