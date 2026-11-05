-- What the completion gates found, one line per gate.
--
-- Kept apart from `error` rather than folded into it: a SUCCEEDED job
-- has a verdict too — the reasoning is worth showing whichever way it
-- went — and a client that renders `error` as a failure would otherwise
-- show every success as one.
--
-- @decision(DL-050)

-- +goose Up
alter table jobs add column verdict text not null default '';

-- +goose Down
alter table jobs drop column verdict;
