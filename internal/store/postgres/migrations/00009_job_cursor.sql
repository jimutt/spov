-- The supervisor's read cursor over a job's events.
--
-- Durable because its conclusions are not idempotent: re-reading an old
-- permission.asked or a human's inbox item re-derives blocked or steered
-- and undoes the explicit act that released the job. Both are legal
-- transitions, so nothing downstream refuses them.
--
-- @decision(DL-046)

-- +goose Up
alter table jobs add column cursor bigint not null default 0;

-- +goose Down
alter table jobs drop column cursor;
