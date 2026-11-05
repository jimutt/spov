-- +goose Up
-- The rows below carry org_id and session_id of their own, because
-- every read filters on them. A composite foreign key makes them agree
-- with the parent row: a message filed under another org or session
-- than its conversation would otherwise be stored and listed there.
--
-- @decision(DL-017)
alter table sessions add constraint sessions_id_org unique (id, org_id);

-- A session's conversation, kept in spov's own database so it can be
-- read after the sandbox that held it is gone. One spov session's
-- sandbox may hold several OpenCode sessions; each is a conversation.
--
-- @decision(DL-017)
create table conversations (
    id          text        primary key,
    org_id      text        not null references orgs (id) on delete cascade,
    session_id  text        not null,
    opencode_id text        not null,
    title       text        not null default '',
    created_at  timestamptz not null,
    updated_at  timestamptz not null,
    unique (session_id, opencode_id),
    unique (id, org_id, session_id),
    foreign key (session_id, org_id) references sessions (id, org_id) on delete cascade
);

-- Keyed on OpenCode's own message id, so a repeated read of the sandbox
-- writes the same row once. `at` is OpenCode's creation time and may be
-- unknown.
--
-- @decision(DL-017)
create table messages (
    id              text        primary key,
    org_id          text        not null references orgs (id) on delete cascade,
    session_id      text        not null references sessions (id) on delete cascade,
    conversation_id text        not null,
    opencode_id     text        not null,
    role            text        not null default '',
    text            text        not null default '',
    at              timestamptz,
    created_at      timestamptz not null,
    unique (conversation_id, opencode_id),
    foreign key (conversation_id, org_id, session_id)
        references conversations (id, org_id, session_id) on delete cascade
);

-- The list page asks every session for its latest message on every
-- refresh of the state stream.
--
-- @decision(DL-017)
create index messages_last on messages (session_id, at desc nulls last, id desc);

-- One durable frame of a conversation's log, or a marker spov wrote
-- into it. `pos` is the store's own strictly increasing position and is
-- what a reader resumes from; `seq` is OpenCode's and is unique only
-- within a conversation. `raw` is the frame as received, so a wrong
-- projection can be redone without a sandbox.
--
-- @decision(DL-017)
create table events (
    id              text        primary key,
    org_id          text        not null references orgs (id) on delete cascade,
    session_id      text        not null references sessions (id) on delete cascade,
    conversation_id text,
    source          text        not null check (source in ('opencode', 'spov')),
    seq             bigint      not null default 0,
    pos             bigint      not null generated always as identity,
    name            text        not null,
    text            text        not null default '',
    at              timestamptz not null,
    raw             bytea       not null default '',
    created_at      timestamptz not null,
    -- A null conversation_id (a spov marker) satisfies the key.
    foreign key (conversation_id, org_id, session_id)
        references conversations (id, org_id, session_id) on delete cascade
);

-- A frame delivered twice is one row. Partial, so spov's own markers —
-- which have no sequence — share the table without inventing one.
--
-- @decision(DL-017)
create unique index events_opencode_seq
    on events (conversation_id, seq)
    where source = 'opencode';

create index events_tail on events (session_id, pos);

-- +goose Down
drop table events;
drop table messages;
drop table conversations;
alter table sessions drop constraint sessions_id_org;
