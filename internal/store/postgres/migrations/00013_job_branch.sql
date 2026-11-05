-- The branch a pr job pushed its work to, and what makes gate 2 a real
-- gate for that kind.
--
-- Without it there is nothing to match a pull request against: a forge
-- may hold dozens, and "some PR exists in this repository" is not
-- evidence about THIS job. Inferring it instead — newest branch, or one
-- created since the job started — would be a guess, and a gate built on
-- a guess is worse than an unverified one because it can be
-- confidently wrong.
--
-- Empty by default, and every existing row stays empty: a job that
-- never reported a branch keeps reporting gate 2 unverified rather than
-- retroactively failing.
--
-- Unlike check_command this is written DURING the run rather than at
-- create, so UpdateJob carries it.
--
-- @decision(DL-051)

-- +goose Up
alter table jobs add column branch text not null default '';

-- +goose Down
alter table jobs drop column branch;
