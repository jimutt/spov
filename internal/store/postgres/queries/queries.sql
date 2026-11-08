-- name: CreateSession :exec
insert into sessions (id, org_id, owner_id, name, state, sandbox_id, request, clones,
                      landing, error, created_at, updated_at, stopped_at, credentials, booted,
                      routed_credentials)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16);

-- name: GetSessionByID :one
select * from sessions where id = $1 and org_id = $2;

-- name: GetLiveSessionByName :one
select * from sessions
where org_id = $1 and name = $2 and state in ('starting', 'running');

-- name: UpdateSession :execrows
update sessions
set owner_id = $3, name = $4, state = $5, sandbox_id = $6, request = $7, clones = $8,
    landing = $9, error = $10, updated_at = $11, stopped_at = $12, credentials = $13, booted = $14,
    routed_credentials = $15
where id = $1 and org_id = $2;

-- name: MarkSessionStopped :execrows
-- @decision(DL-011)
update sessions
set state = $3, stopped_at = $4, error = $5, updated_at = $6
where id = $1 and org_id = $2 and state in ('starting', 'running');

-- name: PutSessionProxyCA :execrows
-- @decision(DL-034)
insert into session_proxy_cas (session_id, org_id, ciphertext, nonce, key_version, created_at)
select s.id, s.org_id, $3, $4, $5, $6 from sessions s where s.id = $1 and s.org_id = $2
on conflict (session_id) do update
set ciphertext = excluded.ciphertext, nonce = excluded.nonce, key_version = excluded.key_version;

-- name: DeleteSessionProxyCA :exec
-- @decision(DL-034)
delete from session_proxy_cas where session_id = $1 and org_id = $2;

-- name: GetSessionProxyCA :one
-- @decision(DL-034)
select ciphertext, nonce, key_version from session_proxy_cas
where session_id = $1 and org_id = $2;

-- name: SetSessionGitToken :execrows
-- @decision(DL-021)
update sessions
set git_token_minted_at = $3, git_token_expires_at = $4, updated_at = $5
where id = $1 and org_id = $2;

-- name: SetSessionOpenCodeGrant :execrows
-- @decision(DL-015)
update sessions
set opencode_grant = $3, updated_at = $4
where id = $1 and org_id = $2;

-- name: GetSessionBySandbox :one
select * from sessions where sandbox_id = $1 and sandbox_id <> '';

-- name: ListLiveSessions :many
select * from sessions where state in ('starting', 'running') order by id;

-- name: CountLiveSessionsForOwner :one
-- The session of an UNFINISHED job is excluded: jobs have their own
-- quota, and counting one against both would let a queue of jobs lock a
-- person out of their own interactive tooling — which is the
-- interference the separate limit exists to prevent.
--
-- The job's state is what decides it, not merely the existence of a job
-- row. A terminal job stops counting against the job quota, so a
-- session still running under one — a terminate that failed, leaving a
-- billing sandbox — would otherwise be counted by neither limit. It
-- falls back to the session quota here, which is where an unowned live
-- sandbox belongs.
--
-- @decision(DL-045)
select count(*) from sessions s
where s.org_id = $1 and s.owner_id = $2 and s.state in ('starting', 'running')
  and not exists (
    select 1 from jobs j
    where j.session_id = s.id and j.org_id = s.org_id
      and j.state not in ('succeeded', 'failed', 'cancelled')
  );

-- name: LockOwnerForQuota :exec
-- Serialises a user's concurrent creates. count(*) takes no locks, so
-- without this two transactions both read "one short of the limit" and
-- both insert; the transaction gives atomicity, not mutual exclusion.
-- Transaction-scoped, so it releases on commit or rollback with no
-- unlock call to leak.
select pg_advisory_xact_lock(hashtext($1), hashtext($2));

-- name: LockUserCredential :exec
-- Serialises the refresh of one user's credential across replicas. The
-- first key is namespaced so it never meets LockOwnerForQuota's.
-- @decision(DL-015)
select pg_advisory_xact_lock(hashtext('credential:' || sqlc.arg(org_id)::text), hashtext(sqlc.arg(user_id)::text || ':' || sqlc.arg(platform)::text));

-- name: LockSession :one
select id from sessions where id = $1 and org_id = $2 for no key update;

-- name: RequestSessionCancel :execrows
update sessions
set cancel_requested_at = $3, updated_at = $3
where id = $1 and org_id = $2 and state = 'starting';

-- name: SessionCancelRequested :one
select coalesce(cancel_requested_at is not null, false)::boolean as cancelled
from sessions where id = $1 and org_id = $2 for update;

-- name: CreateMint :exec
insert into mints (id, org_id, session_id, user_id, port, expires_at, created_at)
values ($1, $2, $3, $4, $5, $6, $7);

-- name: ListMintsBySession :many
select * from mints where org_id = $1 and session_id = $2 order by id;

-- name: CountMintsSince :one
select count(*) from mints where org_id = $1 and session_id = $2 and created_at >= $3;

-- name: SetMintExpiry :execrows
update mints set expires_at = $3 where id = $1 and org_id = $2;

-- name: CreateUser :exec
insert into users (id, org_id, email, name, github_login, admin, git_name, git_email,
                   default_model, quota, last_login_at, created_at, updated_at,
                   default_git_access, default_job_git_access, default_job_duration_seconds)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16);

-- name: GetUser :one
select * from users where id = $1 and org_id = $2;

-- name: GetUserByEmail :one
select * from users where org_id = $1 and lower(email) = lower($2);

-- name: ListUsers :many
select * from users where org_id = $1 order by id;

-- name: UpdateUser :execrows
update users
set email = $3, name = $4, github_login = $5, admin = $6, git_name = $7,
    git_email = $8, default_model = $9, quota = $10, last_login_at = $11,
    updated_at = $12, default_git_access = $13, default_job_git_access = $14,
    default_job_duration_seconds = $15
where id = $1 and org_id = $2;

-- name: DeleteUser :execrows
delete from users where id = $1 and org_id = $2;

-- name: CreateAPIToken :exec
insert into api_tokens (id, org_id, user_id, token_hash, name, last_used_at,
                        expires_at, revoked_at, created_at, updated_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetAPITokenByHash :one
select * from api_tokens where token_hash = $1;

-- name: GetAPIToken :one
select * from api_tokens where id = $1 and org_id = $2;

-- name: ListAPITokensByUser :many
select * from api_tokens where org_id = $1 and user_id = $2 order by id;

-- name: TouchAPIToken :execrows
update api_tokens set last_used_at = $3 where id = $1 and org_id = $2;

-- name: RevokeAPIToken :execrows
update api_tokens
set revoked_at = coalesce(revoked_at, $3), updated_at = $4
where id = $1 and org_id = $2;

-- name: CreateDeviceCode :exec
insert into device_codes (id, org_id, device_code_hash, user_code, user_id, status,
                          name, expires_at, last_polled_at, interval_s, created_at, updated_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: GetDeviceCodeByHash :one
select * from device_codes where device_code_hash = $1;

-- name: GetDeviceCodeByUserCode :one
select * from device_codes where user_code = $1;

-- name: UpdateDeviceCode :execrows
-- org_id is written too: a device code is created in the default org and
-- takes the approving user's org, so omitting it would silently diverge
-- from the memory store the moment a second org exists.
update device_codes
set org_id = $2, user_id = $3, status = $4, name = $5, expires_at = $6,
    last_polled_at = $7, interval_s = $8, updated_at = $9
where id = $1;

-- name: DeleteDeviceCode :execrows
delete from device_codes where id = $1;

-- name: DeleteExpiredDeviceCodes :execrows
delete from device_codes where expires_at < $1;

-- name: PutUserCredential :exec
insert into user_credentials (id, org_id, user_id, platform, ciphertext, nonce,
                              key_version, verified_as, created_at, updated_at,
                              scopes, expires_at, route)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
on conflict (org_id, user_id, platform) do update
set ciphertext = excluded.ciphertext, nonce = excluded.nonce,
    key_version = excluded.key_version, verified_as = excluded.verified_as,
    updated_at = excluded.updated_at, scopes = excluded.scopes,
    expires_at = excluded.expires_at, route = excluded.route;

-- name: GetUserCredential :one
select * from user_credentials where org_id = $1 and user_id = $2 and platform = $3;

-- name: ListUserCredentials :many
select * from user_credentials where org_id = $1 and user_id = $2 order by platform;

-- name: DeleteUserCredential :execrows
delete from user_credentials where org_id = $1 and user_id = $2 and platform = $3;

-- name: ListAllowlist :many
-- collate "C" so the order is the same bytewise order the memory store
-- sorts by: under a locale collation the database ignores punctuation
-- on the first pass, and the two stores would disagree on addresses
-- like a-b@x.com and ab@x.com.
select * from allowlist_entries where org_id = $1 order by email collate "C";

-- name: GetAllowlistEntry :one
select * from allowlist_entries where org_id = $1 and email = $2;

-- name: PutAllowlistEntry :exec
-- created_at is kept on conflict: re-granting admin must not restate
-- when the address was first allowed in.
insert into allowlist_entries (org_id, email, admin, created_at)
values ($1, $2, $3, $4)
on conflict (org_id, email) do update set admin = excluded.admin;

-- name: DeleteAllowlistEntry :execrows
delete from allowlist_entries where org_id = $1 and email = $2;

-- name: CountAllowlistAdmins :one
select count(*) from allowlist_entries where org_id = $1 and admin;

-- name: LockAllowlist :exec
-- Serialises the org's allowlist changes and its first-boot seed.
-- count(*) takes no row locks, so without this two concurrent
-- demotions both see two admins and both proceed, leaving none. The
-- second key is a constant so the lock cannot collide with
-- LockOwnerForQuota's (org, user) pair.
-- Transaction-scoped: it releases on commit or rollback.
select pg_advisory_xact_lock(hashtext($1), hashtext('spov:allowlist'));

-- name: UpsertConversation :one
-- An existing row keeps its id and created_at; the title, the info and
-- the parent change only when the new one says something.
insert into conversations (id, org_id, session_id, opencode_id, title, created_at, updated_at, info, parent_opencode_id)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
on conflict (session_id, opencode_id) do update
set title              = case when excluded.title = '' then conversations.title else excluded.title end,
    info               = coalesce(excluded.info, conversations.info),
    parent_opencode_id = coalesce(excluded.parent_opencode_id, conversations.parent_opencode_id),
    updated_at         = excluded.updated_at
returning *;

-- name: ListConversations :many
-- Oldest first, by created_at rather than by id: a ULID is only
-- monotonic across milliseconds, so two conversations minted in the same
-- one sort by their random suffix. Id breaks the tie so the order is
-- total and stable.
select * from conversations where org_id = $1 and session_id = $2 order by created_at, id;

-- name: PutMessage :execrows
-- The update is conditional so the count reports rows that changed,
-- not rows that were mentioned.
insert into messages (id, org_id, session_id, conversation_id, opencode_id, role, text, at, raw, created_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
on conflict (conversation_id, opencode_id) do update
set role = excluded.role, text = excluded.text, at = excluded.at, raw = excluded.raw
where messages.role is distinct from excluded.role
   or messages.text is distinct from excluded.text
   or messages.at is distinct from excluded.at
   or messages.raw is distinct from excluded.raw;

-- name: ListMessages :many
select * from messages where org_id = $1 and session_id = $2
order by at nulls first, id;

-- name: LastMessages :many
-- Root conversations only: a subagent's message is never a session's
-- preview.
select distinct on (m.session_id) m.* from messages m
join conversations c on c.id = m.conversation_id
where m.org_id = $1 and m.session_id = any(sqlc.arg(session_ids)::text[])
  and c.parent_opencode_id is null
order by m.session_id, m.at desc nulls last, m.id desc;

-- name: InsertEvent :one
-- A frame already stored is not an error and not a row: the caller
-- learns it was skipped by not getting it back.
--
-- `seq` is nullable: a spov marker and an ephemeral frame have none,
-- and a null is absent from the dedup key rather than colliding with
-- every other seq-less row of the conversation.
--
-- @decision(DL-017)
insert into events (id, org_id, session_id, conversation_id, source, seq, name, text, at, raw, created_at)
values (sqlc.arg(id), sqlc.arg(org_id), sqlc.arg(session_id), sqlc.narg(conversation_id),
        sqlc.arg(source), sqlc.narg(seq), sqlc.arg(name), sqlc.arg(text),
        sqlc.arg(at), sqlc.arg(raw), sqlc.arg(created_at))
on conflict (conversation_id, seq) where source = 'opencode' do nothing
returning *;

-- name: ListEvents :many
select * from events where org_id = $1 and session_id = $2 and pos > $3
order by pos limit $4;

-- name: LastEvent :one
select * from events where org_id = $1 and session_id = $2
order by pos desc limit 1;

-- name: FindEvent :one
-- @decision(DL-052)
select * from events where org_id = $1 and session_id = $2 and name = $3
  and position(@contains::bytea in raw) > 0
order by pos desc limit 1;

-- name: SetSeqOffset :execrows
-- @decision(DL-089)
update conversations set seq_base = $3, seq_after = $4 where id = $1 and org_id = $2;

-- name: LastSeqs :many
-- One index probe per conversation, on events_opencode_seq.
-- @decision(DL-017)
select c.id as conversation_id,
       coalesce((select max(e.seq) from events e
         where e.conversation_id = c.id and e.source = 'opencode'), 0)::bigint as last_seq
from conversations c
where c.org_id = $1 and c.session_id = $2;

-- name: LastEventExcept :one
-- @decision(DL-021)
select * from events where org_id = $1 and session_id = $2
  and not (name = any(@skipped_names::text[]))
order by pos desc limit 1;

-- name: CreateJob :exec
-- EVERY column the row carries, not only the ones a job happens to be
-- created with. A column left out of this insert silently takes its
-- default, because cursor, verdict, branch and assessment all default to
-- a zero value rather than refusing. The one production caller sets none
-- of them; the contract test is what checks them.
insert into jobs (id, org_id, owner_id, session_id, kind, state, goal, unfinished, error,
                  attempt, created_at, updated_at, started_at, stopped_at, check_command,
                  check_proposed, cursor, verdict, assessment, model,
                  max_duration_seconds, clock_started_at, clock_used_seconds)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        $19, $20, $21, $22, $23);

-- name: GetJob :one
select * from jobs where org_id = $1 and id = $2;

-- name: GetJobForUpdate :one
-- The row lock is what makes read-then-write one step: the supervisor
-- runs behind the leader lock, but a restart or a failover can leave
-- two replicas briefly both believing they hold it, and the transition
-- check would otherwise be decided on a stale read.
select * from jobs where org_id = $1 and id = $2 for update;

-- name: GetJobBySession :one
select * from jobs where org_id = $1 and session_id = $2;

-- name: JobsBySessions :many
-- The session list's decoration, in one query for the page rather than
-- one per row. session_id is unique among a session's jobs (the
-- composite foreign key), so `distinct on` returns at most one each;
-- the id tie-break makes a corrupt store answer consistently rather
-- than differently on each request.
--
-- @decision(DL-060)
select distinct on (session_id) * from jobs
where org_id = $1 and session_id = any(sqlc.arg(session_ids)::text[])
order by session_id, id desc;

-- name: ListJobs :many
-- Newest first, one page: rows below `before`, at most `row_limit`. A
-- null limit is no limit.
select * from jobs
where org_id = $1
  and (sqlc.narg(session_id)::text is null or session_id = sqlc.narg(session_id)::text)
  and (sqlc.narg(owner_id)::text is null or owner_id = sqlc.narg(owner_id)::text)
  and (cardinality(sqlc.arg(states)::text[]) = 0 or state = any(sqlc.arg(states)::text[]))
  and (sqlc.narg(before)::text is null or id < sqlc.narg(before)::text)
order by id desc
limit sqlc.narg(row_limit)::int;

-- name: CountJobs :one
select count(*) from jobs
where org_id = $1
  and (sqlc.narg(session_id)::text is null or session_id = sqlc.narg(session_id)::text)
  and (sqlc.narg(owner_id)::text is null or owner_id = sqlc.narg(owner_id)::text)
  and (cardinality(sqlc.arg(states)::text[]) = 0 or state = any(sqlc.arg(states)::text[]));

-- name: CountLiveJobsForOwner :one
-- Every state that is not terminal, `queued` included: a queued job
-- holds no sandbox yet but is committed to getting one.
select count(*) from jobs
where org_id = $1 and owner_id = $2
  and state not in ('succeeded', 'failed', 'cancelled');

-- name: LockOwnerForJobQuota :exec
-- LockOwnerForQuota's counterpart for jobs, and deliberately a
-- different key: jobs and sessions have separate quotas, so a job
-- create and a session create by the same user must not serialise on
-- each other. The caller passes an already-prefixed owner key (see
-- pgJobs.CountLive), which is what separates the two keyspaces —
-- concatenating here would make the parameter nullable, and
-- hashtext(null) is null, which pg_advisory_xact_lock refuses.
-- Transaction-scoped, so it releases on commit or rollback.
select pg_advisory_xact_lock(hashtext($1), hashtext($2));

-- name: ListLiveJobs :many
-- The supervisor's restart query, and the only job listing with no org:
-- a restarted replica has no principal to scope by.
select * from jobs
where state not in ('succeeded', 'failed', 'cancelled')
order by id desc;

-- name: UpdateJob :execrows
update jobs
set state = $3, goal = $4, unfinished = $5, error = $6, attempt = $7,
    updated_at = $8, started_at = $9, stopped_at = $10, cursor = $11,
    verdict = $12, assessment = $13, check_command = $14,
    check_proposed = $15, clock_started_at = $16, clock_used_seconds = $17
where org_id = $1 and id = $2;

-- name: PutDeliverable :execrows
-- A member is inserted, or updated unless the stored one is verified, so
-- a later write never makes it worse. The org check rides on the job.
-- @decision(DL-051)
insert into job_deliverables (org_id, job_id, key, kind, status, detail, platform, owner,
                              name, branch, number, url, title, path, size, created_at, updated_at,
                              forge, images_lost)
select j.org_id, j.id, @key, @kind, @status, @detail, @platform, @owner, @name, @branch,
       @number, @url, @title, @path, @size, @now, @now, @forge, @images_lost
from jobs j where j.org_id = @org_id and j.id = @job_id
on conflict (job_id, key) do update
set status = excluded.status, detail = excluded.detail, platform = excluded.platform,
    owner = excluded.owner, name = excluded.name, branch = excluded.branch,
    number = excluded.number, url = excluded.url, title = excluded.title,
    path = excluded.path, size = excluded.size, updated_at = excluded.updated_at,
    forge = case when excluded.forge <> '' then excluded.forge else job_deliverables.forge end,
    images_lost = excluded.images_lost
where job_deliverables.status <> 'verified';

-- name: JobExists :one
select exists (select 1 from jobs where org_id = $1 and id = $2);

-- name: DeliverablesForJobs :many
-- @decision(DL-051)
select * from job_deliverables
where org_id = $1 and job_id = any(sqlc.arg(job_ids)::text[])
order by job_id, key;

-- name: PutJobFile :execrows
-- Inserted once: a stored file is never replaced.
-- @decision(DL-051)
insert into job_files (org_id, job_id, path, content_type, size, body, created_at)
select j.org_id, j.id, @path, @content_type, @size, @body, @now
from jobs j where j.org_id = @org_id and j.id = @job_id
on conflict (job_id, path) do nothing;

-- name: GetJobFile :one
select * from job_files where org_id = $1 and job_id = $2 and path = $3;

-- name: ListJobFiles :many
select org_id, job_id, path, content_type, size, created_at from job_files
where org_id = $1 and job_id = $2
order by path;

-- name: RecordImage :exec
-- @decision(DL-016)
insert into images (name, image_id, kind, request_key, versions, published_at, last_used_at, deleted_at)
values ($1, $2, $3, $4, $5, $6, $7, null)
on conflict (name) do update
set image_id = excluded.image_id, kind = excluded.kind, request_key = excluded.request_key,
    versions = excluded.versions, published_at = excluded.published_at,
    last_used_at = excluded.last_used_at, deleted_at = null;

-- name: GetImage :one
select * from images where name = $1;

-- name: NewestImage :one
-- @decision(DL-016)
select * from images
where kind = 'variant' and request_key = $1 and deleted_at is null
order by published_at desc
limit 1;

-- name: UseImage :exec
-- Enters an unknown name as published at its first use; moves a known,
-- undeleted one's last use forward.
--
-- @decision(DL-016)
insert into images (name, image_id, kind, request_key, versions, published_at, last_used_at)
values ($1, $2, $3, $4, $5, $6, $6)
on conflict (name) do update
set last_used_at = greatest(images.last_used_at, excluded.last_used_at)
where images.deleted_at is null;

-- name: ListImages :many
select * from images order by name;

-- name: MarkImageDeleted :execrows
-- @decision(DL-016)
update images set deleted_at = $2 where name = $1;

-- name: GetImagePrune :one
select pruned_at from image_prunes where id;

-- name: SetImagePrune :exec
insert into image_prunes (id, pruned_at) values (true, $1)
on conflict (id) do update set pruned_at = excluded.pruned_at;

-- name: PutOrgCredential :exec
insert into org_credentials (id, org_id, platform, ciphertext, nonce, key_version,
                             meta, updated_by, created_at, updated_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
on conflict (org_id, platform) do update
set ciphertext = excluded.ciphertext, nonce = excluded.nonce,
    key_version = excluded.key_version, meta = excluded.meta,
    updated_by = excluded.updated_by, updated_at = excluded.updated_at;

-- name: GetOrgCredential :one
select * from org_credentials where org_id = $1 and platform = $2;

-- name: ListOrgCredentials :many
-- @decision(DL-074)
select * from org_credentials where org_id = $1 order by platform;

-- name: DeleteOrgCredential :execrows
delete from org_credentials where org_id = $1 and platform = $2;

-- name: AppendAdminEvent :exec
insert into admin_events (id, org_id, actor, action, detail, at)
values ($1, $2, $3, $4, $5, $6);

-- name: ListAdminEvents :many
select * from admin_events where org_id = $1 order by at desc, id desc limit $2;

-- name: InsertRun :execrows
-- @decision(DL-085)
insert into session_runs (session_id, org_id, owner_id, n, sandbox_id, booted, booted_image_id, opencode,
                          origin, started_at, running_at, ended_at, snapshot, snapshot_state, snapshot_at, first_message)
select s.id, s.org_id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
from sessions s where s.id = $1 and s.org_id = $2;

-- name: UpdateRun :execrows
-- @decision(DL-085)
update session_runs
set sandbox_id = $4, booted = $5, booted_image_id = $6, opencode = $7, origin = $8,
    running_at = $9, ended_at = $10, snapshot = $11, snapshot_state = $12, snapshot_at = $13, first_message = $14
where session_id = $1 and org_id = $2 and n = $3;

-- name: CurrentRun :one
-- @decision(DL-085)
select * from session_runs where session_id = $1 and org_id = $2 order by n desc limit 1;

-- name: ListRuns :many
-- @decision(DL-085)
select * from session_runs where org_id = $1 and session_id = any($2::text[]) order by session_id, n;

-- name: SnapshotRuns :many
-- @decision(DL-085)
select * from session_runs
where ended_at is not null and snapshot_state = any($1::text[])
order by ended_at, session_id, n;

-- name: EndCurrentRun :exec
-- @decision(DL-085)
update session_runs u set ended_at = $3
where u.session_id = $1 and u.org_id = $2 and u.ended_at is null
  and u.n = (select max(r.n) from session_runs r where r.session_id = u.session_id);

-- name: ReopenCurrentRun :execrows
-- @decision(DL-085)
update session_runs u
set ended_at = null,
    snapshot_state = case when u.snapshot_state = 'failed' then 'pending' else u.snapshot_state end
where u.session_id = $1 and u.org_id = $2
  and u.n = (select max(r.n) from session_runs r where r.session_id = u.session_id);
