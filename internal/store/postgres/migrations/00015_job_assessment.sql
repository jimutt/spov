-- The preparation pass's reading of the goal.
--
-- It is a separate column rather than an edit to `goal` because the
-- goal is canonical and this is derived: re-running the pass must be
-- possible without rewriting what the user asked for, and a row whose
-- goal had been overwritten could not say what was originally asked.
--
-- It is stored rather than held in memory for two reasons. A job is
-- created on one replica and provisioned by whichever supervisor picks
-- it up, so the pass's result has to survive the handover. And River
-- redelivers: an assessment that lived only in the worker would be paid
-- for again on every retry.
--
-- Empty by default, and every existing row stays empty: such a job
-- opens on its goal alone, which is what every job did before this.
--
-- @decision(DL-047)

-- +goose Up
alter table jobs add column assessment text not null default '';

-- +goose Down
alter table jobs drop column assessment;
