-- A pull request member's forge kind, so a view can name it the forge's
-- way: a merge request on GitLab, a pull request elsewhere.
--
-- @decision(DL-051)

-- +goose Up
alter table job_deliverables add column forge text not null default '';

-- +goose Down
alter table job_deliverables drop column forge;
