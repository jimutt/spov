# Metrics

`spov serve` can publish control-plane metrics through OpenTelemetry. They show
whether work is progressing, where it fails, and whether cleanup keeps up. They
are operational counts only: no label carries an ID, a name, a goal, a URL, a
repository or error text ([DL-011](../decisions/records/server/DL-011.md)).

## Turning them on

Nothing is exported unless you configure it
([DL-011](../decisions/records/server/DL-011.md)), and a server the CLI spawns
locally (`spov serve --local`) never exports.

- **Prometheus.** Set `metrics.prometheus_listen` in the server's config, or
  `SPOV_METRICS_PROMETHEUS_LISTEN`, to an address such as `127.0.0.1:9464`. The
  server then serves `GET /metrics` on a listener of its own, separate from the
  API port, with no authentication. Keep it on a loopback or private address.
- **OTLP.** Set `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` or
  `OTEL_EXPORTER_OTLP_ENDPOINT`. The exporter speaks OTLP over HTTP
  (`http/protobuf`) and reads the other standard variables itself:
  `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, the TLS settings,
  and `OTEL_METRIC_EXPORT_INTERVAL` (60 s by default).

Only OTLP over HTTP is offered. An `OTEL_EXPORTER_OTLP_PROTOCOL` of `grpc` is
ignored, with a warning at startup. The exporter reads its variables from the
process environment.

Both can run at once. A listener that cannot bind, or a collector that cannot be
reached, is logged (export errors at most every 15 minutes) and never stops the
server.

Every series carries the resource attributes `service.name=spov` (fixed;
`OTEL_SERVICE_NAME` does not rename it), `service.version` and
`service.instance.id`. The last is random per process, so
one replica's series stay apart from another's. Over Prometheus they appear on
`target_info`.

## Per process or shared

Most metrics are **per process**: each replica counts what it observed, and you
sum across replicas. The three state gauges (`spov_jobs`, `spov_sessions` and
`spov_job_oldest_queued_age`) are **shared**. Only the replica holding the
reconciler lock reports them, from a snapshot it refreshes every 30 seconds, so
there is one series and you must not sum it across instances. Use `max`. When
leadership moves, they can be missing for up to one refresh, or briefly reported
by both the old and the new holder
([DL-011](../decisions/records/server/DL-011.md)).

A job's ending, and a session's stop, is counted once, by the replica whose
write moved it there. A redelivered or retried attempt counts nothing. The one
exception is a session the reconciler marks ended after a transient listing miss
and then brings back: its real stop is counted again.

## The catalogue

Names are as Prometheus scrapes them. Over OTLP the same instruments arrive
without the `_total` and unit suffixes, for example `spov_api_requests`. Times
are in seconds.

| Prometheus name | Type | Labels | Scope | What it tells you |
|---|---|---|---|---|
| `spov_build_info` | gauge | `version` | per process | The running build, always 1. |
| `spov_api_requests_total` | counter | `operation`, `status_class` | per process | API traffic and errors by operation (the huma operation ID) and `2xx`/`3xx`/`4xx`/`5xx`. Requests refused before they reach an operation (a missing or bad token, the browser guard) are not counted. Event streams are counted when they end. |
| `spov_api_request_duration_seconds` | histogram | `operation` | per process | API latency by operation. |
| `spov_quota_refusals_total` | counter | `limit` | per process | Requests refused by a user's limits: `sessions`, `jobs`, `size`, `duration`, `image_builds`. |
| `spov_requests_limited_total` | counter | `limit` | per process | Requests refused by the server's request limits ([Limits](server.md#limits)): `sign_in_rate` and `device_poll_rate` (per client address or network), `sign_in_cap` and `device_code_cap` (sign-ins in progress on the replica), `sign_in_share` and `device_code_share` (one network holding its most places in a cap), `requests`, `creates`, `streams` and `in_flight` (per user). A steady `sign_in_rate` from one address behind a reverse proxy usually means `server.trusted_proxies` is unset. |
| `spov_jobs` | gauge | `state` | shared | Unfinished jobs by state: `queued`, `provisioning`, `running`, `blocked`, `steered`, `delivering`. |
| `spov_job_oldest_queued_age_seconds` | gauge | none | shared | How long the oldest queued job has waited. Rising means jobs are not being picked up. |
| `spov_job_endings_total` | counter | `kind`, `outcome`, `ended_by` | per process | Finished jobs. `kind` is `pr`, `report` or `sync`. `outcome` is `succeeded`, `failed` or `cancelled`. `ended_by` is `declared`, `wall-clock`, `stopped`, `denied`, `no-completion-tool`, `continue-failed`, `turn-failed`, `abandoned`, `cancelled`, `launch-failed` or `other`. |
| `spov_job_queue_delay_seconds` | histogram | none | per process | Time from a job's creation to its provisioning. |
| `spov_job_clock_used_seconds` | histogram | `kind` | per process | The wall clock a job used, recorded when it ends; a job whose clock never started records none. |
| `spov_job_delivery_duration_seconds` | histogram | none | per process | How long the delivering pass that concluded a judged job took: collecting, judging and stopping the sandbox. Waiting to be picked up, and earlier attempts, are not included. |
| `spov_job_gates_total` | counter | `gate`, `result` | per process | Verdict gates (`check`, `deliverable`, `conduct`) by `passed`, `partial`, `failed`, `unverified`. `partial` is the deliverable gate only. |
| `spov_job_deliverables_total` | counter | `kind`, `status` | per process | A judged job's deliverables, recorded with its ending. `kind` is `pr`, `report` or `sync`; `status` is `verified`, `missing`, `unverified`, `over_limit` or `withdrawn`. Rising `unverified` means spov could not ask a forge or read a sandbox. |
| `spov_sessions` | gauge | `state`, `kind` | shared | Live sessions by state (`starting`, `running`) and kind (`interactive`, `job`). |
| `spov_session_launches_total` | counter | `kind`, `outcome`, `failed_step` | per process | Launches by `kind` and `outcome` (`succeeded` or `failed`), and at which step: `setup` (before the sandbox: credentials, image, add-ons), `tokens`, `sandbox`, `server` (waiting for OpenCode to answer), or `none` on success. A launch cancelled while starting is in neither. |
| `spov_session_launch_duration_seconds` | histogram | `kind`, `outcome` | per process | Launch time, by `kind` and `outcome` as above. |
| `spov_session_lifetime_seconds` | histogram | `kind` | per process | How long sessions lived, recorded when they stop: the basis for estimated billable time. A job's session counts from the job's start, so queueing is left out; a job that never started records none. A launch that failed or was cancelled while starting records none. |
| `spov_session_terminations_total` | counter | `reason` | per process | Sessions that stopped: `stopped` (a person, a job's end or a cancel asked), `ended` (the sandbox ended on its own, at its idle timeout or lifetime), `reaped` (the age backstop terminated a sandbox past the maximum session age), `failed` (a launch the server lost while it was starting, which only the reconciler records), `released` (a held sync job's sandbox ended by `spov jobs apply` or `spov jobs release`). |
| `spov_reconcile_runs_total` | counter | `outcome` | per process | Reconciler passes, `ok` or `error`. Only the lock holder runs them. |
| `spov_reconcile_duration_seconds` | histogram | none | per process | Reconciler pass time. |
| `spov_supervisor_passes_total` | counter | `outcome` | per process | Supervisor passes over a job, `ok` or `error`. |
| `spov_recorder_gaps_total` | counter | none | per process | Gaps marked in a session's recorded log. |
| `spov_recorder_reconnects_total` | counter | none | per process | Times the recorder reattached to a session's event stream. |
| `spov_git_token_renewals_total` | counter | `outcome` | per process | Git token re-mints: `renewed`, `skipped`, `failed`. |
| `spov_proxy_recoveries_total` | counter | `outcome` | per process | Credential proxy sidecars recreated (`recreated`), or sessions ended because a recreated sidecar died again (`failed`). An attempt that fails and is retried next minute is not counted. |
| `spov_image_builds_total` | counter | `outcome` | per process | Sandbox image builds, `ok` or `error`. |
| `spov_snapshot_collections_total` | counter | `outcome` | per process | Exit snapshots settled after a run ended: `kept`, `failed` (Modal took none, or it was still pending 30 minutes after the end), `none` (the sandbox was created without one). Only the reconciler's lock holder, or a resume waiting for one, collects. |
| `spov_snapshot_prunes_total` | counter | `reason` | per process | Exit snapshots marked deleted: `retention`, `cap` (the owner's oldest beyond `snapshots.max_per_user`), `superseded` (a later run of the session kept its own), `unused` (its run never reached running). |

A label value outside its listed set is recorded as `other`.

`TestEveryMetricIsDocumented` in `internal/telemetry` fails when a metric or a
label key is missing from this table.

## Dashboard

[`deploy/grafana/spov-dashboard.json`](../deploy/grafana/spov-dashboard.json) is
an importable Grafana dashboard over the Prometheus names. It has three rows:
work progressing, failures, and cleanup health. Import it, pick your Prometheus
data source, and it works for one replica or several.
