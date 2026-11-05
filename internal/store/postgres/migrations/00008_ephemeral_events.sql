-- +goose Up
-- Permission frames and the raw message body: the two things the job
-- model needs that DL-017 deliberately did not keep.
--
-- @decision(DL-017)

-- `seq` becomes nullable, and that is the whole of why this is a
-- migration rather than a filter branch.
--
-- An ephemeral frame carries no `durable` object and therefore no
-- sequence. Stored with seq 0 it would collide with every other
-- seq-less row of its conversation under events_opencode_seq, and the
-- row swallowed would be the *second* permission.asked — the one the
-- supervisor exists to notice. NULLs are distinct in a unique index,
-- so a null seq removes the row from the dedup key entirely while a
-- real one is still deduplicated exactly as before.
--
-- The existing rows are spov's own markers, which use 0 for "none";
-- they are moved to null so one value means one thing.
--
-- @decision(DL-017)
alter table events alter column seq drop not null;
alter table events alter column seq drop default;
update events set seq = null where seq = 0;

-- A third source, rather than writing these as 'spov'. DL-018 makes
-- `source` what marks a row as spov's own voice — the page draws a seam
-- from `source === "spov"` alone — so a sandbox-authored frame wearing
-- it would speak in spov's voice on the page. 'ephemeral' says what the
-- row is (a frame OpenCode did not keep) without claiming who said it.
--
-- @decision(DL-017) @decision(DL-018)
alter table events drop constraint events_source_check;
alter table events add constraint events_source_check
    check (source in ('opencode', 'spov', 'ephemeral'));

-- The dedup key is unchanged in shape and now excludes ephemeral rows
-- twice over: by source, and by their null seq. Recreated rather than
-- left alone because the column under it changed nullability.
drop index events_opencode_seq;
create unique index events_opencode_seq
    on events (conversation_id, seq)
    where source = 'opencode';

-- The supervisor's own query: a session's ephemeral frames in order.
-- Without it, finding an unanswered permission means scanning the
-- session's whole log, which is the busiest table spov has.
--
-- @decision(DL-017)
create index events_ephemeral on events (session_id, pos) where source = 'ephemeral';

-- The message as received, for the same reason events.raw exists: the
-- `/message` shape is inferred (AGENTS.md), and DL-050's gate 3 needs
-- tool parts that the {role, text, at} projection does not keep. Kept
-- raw so a wrong projection can be redone without a sandbox — which
-- matters most for a job whose sandbox is already terminated.
--
-- @decision(DL-017) @decision(DL-050)
alter table messages add column raw bytea not null default '';

-- +goose Down
alter table messages drop column raw;
drop index events_ephemeral;
drop index events_opencode_seq;
alter table events drop constraint events_source_check;
alter table events add constraint events_source_check
    check (source in ('opencode', 'spov'));
update events set seq = 0 where seq is null;
alter table events alter column seq set default 0;
alter table events alter column seq set not null;
create unique index events_opencode_seq
    on events (conversation_id, seq)
    where source = 'opencode';
