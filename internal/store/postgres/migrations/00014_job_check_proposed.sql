-- Whether the check command came from the caller or from spov's own
-- preparation pass.
--
-- The two are not equivalent evidence. A gate that passed on a check
-- the caller named is a statement about the repository's own
-- standard; one that passed on a check spov worked out for itself is a
-- statement about spov's reading of the request. A reader must be able
-- to tell which they are looking at, and it cannot be recovered later:
-- once the command is on the row, nothing about the string says where
-- it came from.
--
-- False by default, and every existing row stays false: every check
-- that exists today was given by a caller, since nothing else could
-- have written one.
--
-- Written at create alongside check_command rather than during the run,
-- because the pass runs before provisioning.
--
-- @decision(DL-047) @decision(DL-050)

-- +goose Up
alter table jobs add column check_proposed boolean not null default false;

-- +goose Down
alter table jobs drop column check_proposed;
