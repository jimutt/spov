-- What a session's sandbox said each model offers as reasoning modes,
-- with the sandbox it was read from. A table of its own, so no session
-- read or listing carries it.
--
-- @decision(DL-159)

-- +goose Up
create table session_model_variants (
    session_id text        primary key,
    org_id     text        not null,
    sandbox_id text        not null,
    models     jsonb       not null default '{}'::jsonb,
    updated_at timestamptz not null,
    foreign key (session_id, org_id) references sessions (id, org_id) on delete cascade
);

-- +goose Down
drop table session_model_variants;
