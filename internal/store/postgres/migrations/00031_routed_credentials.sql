-- A routed credential's route, beside its sealed secret: the hosts,
-- paths and scheme it is set on, in the clear so a listing can show it.
-- The seal binds it, so a route changed here no longer opens. Null on
-- every other credential.
--
-- @decision(DL-073)

-- +goose Up
alter table user_credentials add column route jsonb;

-- +goose Down
alter table user_credentials drop column route;
