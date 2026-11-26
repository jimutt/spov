-- The newest reasoning modes an org's sandboxes reported for each
-- model, for one OpenCode version: one row per org and version, the
-- models a jsonb object keyed provider/model.
--
-- @decision(DL-186)

-- +goose Up
create table model_modes_seen (
    org_id     text        not null,
    opencode   text        not null,
    models     jsonb       not null default '{}'::jsonb,
    updated_at timestamptz not null,
    primary key (org_id, opencode)
);

-- +goose Down
drop table model_modes_seen;
