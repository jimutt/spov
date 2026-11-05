-- +goose Up
-- A launch runs on one replica but may be deleted from any of them, and
-- the pre-sandbox window is exactly when there is no sandbox id to
-- terminate through. A cancel flag in the launching process's memory
-- would miss a delete that landed elsewhere: it would mark the row
-- stopped and the launch — reading its own empty map — would write it
-- back to running with a live sandbox, billing on after the user was
-- told the session was gone.
--
-- The launch re-reads this immediately before it commits, under the
-- store's own row lock, which is what makes cancel and commit mutually
-- exclusive across processes rather than only within one.
--
-- @decision(DL-003)
alter table sessions add column cancel_requested_at timestamptz;

-- +goose Down
alter table sessions drop column cancel_requested_at;
