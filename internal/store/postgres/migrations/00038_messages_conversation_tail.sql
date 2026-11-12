-- A conversation's newest messages, read without walking its session's
-- other conversations: the supervisor reads the newest of a job's own
-- conversation on every pass.
--
-- @decision(DL-131)

-- +goose Up
create index messages_conversation_tail on messages (conversation_id, at desc nulls last, id desc);

-- +goose Down
drop index messages_conversation_tail;
