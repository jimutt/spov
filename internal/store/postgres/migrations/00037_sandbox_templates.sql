-- A user's sandbox templates: named sets of sandbox settings a launch
-- may take. The settings are one JSON document and hold no secret.
--
-- @decision(DL-119)

-- +goose Up
create table sandbox_templates (
    org_id     text        not null references orgs (id) on delete cascade,
    user_id    text        not null,
    name       text        not null,
    settings   jsonb       not null default '{}'::jsonb,
    created_at timestamptz not null,
    updated_at timestamptz not null,
    primary key (org_id, user_id, name)
);

-- +goose Down
drop table sandbox_templates;
