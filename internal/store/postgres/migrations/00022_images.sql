-- The image registry: every image spov published, when a launch last
-- used it and whether pruning deleted it (DL-016). A variant's
-- request_key is its name with its add-on versions as they were asked
-- for, and versions its add-ons as installed. Deployment-wide, like the
-- images themselves.
--
-- A session's booted column is the image its sandbox was created from
-- and the add-ons it installs at their exact versions (DL-033).
--
-- @decision(DL-033) @decision(DL-016)

-- +goose Up
create table images (
    name         text primary key,
    image_id     text not null default '',
    kind         text not null check (kind in ('base', 'proxy', 'variant')),
    request_key  text not null default '',
    versions     jsonb not null default '[]',
    published_at timestamptz not null,
    last_used_at timestamptz not null,
    deleted_at   timestamptz
);
create index images_newest on images (request_key, published_at desc)
    where kind = 'variant' and deleted_at is null;

-- One row: when pruning last ran.
create table image_prunes (
    id        boolean primary key default true check (id),
    pruned_at timestamptz not null
);

alter table sessions add column booted jsonb not null default '{}';

-- +goose Down
alter table sessions drop column booted;
drop table image_prunes;
drop table images;
