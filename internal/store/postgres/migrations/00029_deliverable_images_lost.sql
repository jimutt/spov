-- A report member that lost some of its linked images, which gate 2
-- reads as a partial delivery.
--
-- @decision(DL-051)

-- +goose Up
alter table job_deliverables add column images_lost boolean not null default false;

-- +goose Down
alter table job_deliverables drop column images_lost;
