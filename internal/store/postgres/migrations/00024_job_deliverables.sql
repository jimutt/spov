-- A job's deliverables are an inventory, one row per member, and what
-- delivery collected is kept apart from it, one row per file, so a
-- listing never loads a body. A member's key is its identity within the
-- job: pr:<platform>:<owner>/<name>:<branch> or report:<path>.
--
-- jobs.result and jobs.branch are moved in here and dropped. A pr job's
-- URL was written only once gate 2 had found its pull request, so it
-- becomes a verified member on the session's first repository, the only
-- one gate 2 asked. A report's body becomes a verified report.md.
--
-- @decision(DL-051)

-- +goose Up
create table job_deliverables (
    org_id     text        not null,
    job_id     text        not null references jobs (id) on delete cascade,
    key        text        not null,
    kind       text        not null check (kind in ('pr', 'report')),
    status     text        not null check (status in ('verified', 'missing', 'unverified', 'over_limit')),
    detail     text        not null default '',
    platform   text        not null default '',
    owner      text        not null default '',
    name       text        not null default '',
    branch     text        not null default '',
    number     integer     not null default 0,
    url        text        not null default '',
    title      text        not null default '',
    path       text        not null default '',
    size       bigint      not null default 0,
    created_at timestamptz not null,
    updated_at timestamptz not null,
    primary key (job_id, key)
);

create table job_files (
    org_id       text        not null,
    job_id       text        not null references jobs (id) on delete cascade,
    path         text        not null,
    content_type text        not null,
    size         bigint      not null,
    body         bytea       not null,
    created_at   timestamptz not null,
    primary key (job_id, path)
);

alter table jobs add column unfinished text not null default '';

insert into job_deliverables
    (org_id, job_id, key, kind, status, detail, platform, owner, name, branch, url, created_at, updated_at)
select j.org_id, j.id,
       'pr:' || coalesce(s.request->'Repos'->0->>'Platform', '') || ':' ||
           coalesce(s.request->'Repos'->0->>'Owner', '') || '/' ||
           coalesce(s.request->'Repos'->0->>'Name', '') || ':' || j.branch,
       'pr',
       case when j.result <> '' then 'verified' else 'unverified' end,
       case when j.result <> '' then ''
            else 'recorded before deliverables were tracked' end,
       coalesce(s.request->'Repos'->0->>'Platform', ''),
       coalesce(s.request->'Repos'->0->>'Owner', ''),
       coalesce(s.request->'Repos'->0->>'Name', ''),
       j.branch, j.result, j.updated_at, j.updated_at
from jobs j
join sessions s on s.id = j.session_id and s.org_id = j.org_id
where j.kind = 'pr' and (j.result <> '' or j.branch <> '');

insert into job_files (org_id, job_id, path, content_type, size, body, created_at)
select org_id, id, 'report.md', 'text/markdown; charset=utf-8',
       octet_length(convert_to(result, 'UTF8')), convert_to(result, 'UTF8'), updated_at
from jobs
where kind = 'report' and result <> '';

insert into job_deliverables
    (org_id, job_id, key, kind, status, title, path, size, created_at, updated_at)
select org_id, id, 'report:report.md', 'report', 'verified',
       coalesce(nullif(btrim(regexp_replace(
           substring(result from '(?n)^ {0,3}# +([^\r\n]+)'), '\s+#+\s*$', '')), ''), 'report'),
       'report.md', octet_length(convert_to(result, 'UTF8')), updated_at, updated_at
from jobs
where kind = 'report' and result <> '';

alter table jobs drop column result;
alter table jobs drop column branch;

-- +goose Down
alter table jobs add column result text not null default '';
alter table jobs add column branch text not null default '';
alter table jobs drop column unfinished;
drop table job_files;
drop table job_deliverables;
