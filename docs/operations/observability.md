# Observability

## Endpoints

Every process (`serve`, `worker`, `mirror worker`) runs an operations listener on `TEAM_MEMORY_METRICS_LISTEN` (default `127.0.0.1:9090`; the image and chart use `:9090`). The ingress never routes it, and the chart's NetworkPolicy admits only `networkPolicy.metricsFrom`.

| Path | serve | workers |
| --- | --- | --- |
| `/metrics` | Prometheus exposition | same |
| `/healthz` | liveness (process up) | liveness: fails when the worker loop has not polled the queue for a lease plus 2 minutes (stuck, not idle) |
| `/readyz` | database reachable and, with SSO, every provider usable; fails while shutting down | database reachable |

`serve` also answers `/healthz` and `/readyz` on its main port (the chart's probes use those).

## Metrics

All names start with `team_memory_`. Labels are bounded: routes are normalized, tool names come from the fixed set of eight.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `build_info` | gauge | `component`, `version` | 1 per process |
| `ready` | gauge | | 1 when the last readiness check passed |
| `http_requests_total` | counter | `route`, `method`, `code` (class) | `/mcp`, `/oauth`, `/.well-known`, `/portal`, `/healthz`, `/readyz`, `other` |
| `http_request_duration_seconds` | histogram | `route` | |
| `mcp_tool_calls_total` | counter | `tool`, `outcome` (`ok`, `tool_error`, `error`) | |
| `mcp_tool_duration_seconds` | histogram | `tool` | includes query embedding for `memory_search` |
| `embedding_duration_seconds` / `embedding_errors_total` | histogram / counter | `model`, `purpose` (`query`, `document`) | Ollama calls |
| `db_pool_*` | gauges and counters | `pool` (`app`, `worker`, `mirror`) | acquired, idle, total, max connections; waits and wait time |
| `jobs` | gauge | `kind`, `status` | queue depth, read at scrape time (each worker sees its own kinds) |
| `jobs_oldest_pending_age_seconds` | gauge | `kind` | backlog age |
| `jobs_claimed_total`, `jobs_succeeded_total`, `jobs_retried_total`, `jobs_dead_lettered_total`, `jobs_reaped_total` | counters | `kind` | |
| `job_batch_duration_seconds` | histogram | `kind` | |
| `worker_last_poll_timestamp_seconds` | gauge | `kind` | |
| `mirror_exports_total`, `mirror_commits_total`, `mirror_push_failures_total`, `mirror_secret_detected_total`, `mirror_held_total` | counters | `repository` (scope id) | |
| `mirror_export_duration_seconds` | histogram | | |
| `mirror_undelivered` | gauge | `state` | events not yet pushed |

Plus the standard Go runtime and process collectors.

Scrape with the Prometheus Operator (`metrics.serviceMonitor.enabled=true`; the chart's headless `<release>-metrics` Service selects every component) or with plain Prometheus using DNS service discovery on that Service (`deploy/dev-k3d/observability.yaml` is an example).

## Tracing

OpenTelemetry tracing is always on in-process; spans are **exported** only when `OTEL_EXPORTER_OTLP_ENDPOINT` (chart: `tracing.otlpEndpoint`) points at an OTLP/HTTP collector. Standard variables apply: `OTEL_TRACES_SAMPLER` (chart default `parentbased_traceidratio`), `OTEL_TRACES_SAMPLER_ARG` (0.1), `OTEL_RESOURCE_ATTRIBUTES`. Services are `team-memory-serve`, `team-memory-worker`, `team-memory-mirror`.

Spans: one server span per HTTP request (continuing an incoming W3C `traceparent`), `mcp.tool <name>`, a client span per database statement (named after the sqlc query, never with parameters), the Ollama HTTP call, and `jobs.batch <kind>` in workers.

## Request ids, logs and audit

Each request gets an id: a valid incoming `X-Request-Id`, else the **trace id**, else a random one. It is returned in `X-Request-Id` (and the trace id in `X-Trace-Id`), logged on the request log line (`request_id`, `trace_id`), and stored in every audit event the request writes (`audit_events.request_id`). So a user's error report with the response header leads to the log line, the trace and the audit rows. Logs are JSON on stderr and never contain credentials or request bodies.

## Alerts

`deploy/helm/team-memory/files/prometheus-rules.yaml` (17 rules in 5 groups): readiness, absence, 5xx ratio, search and write latency against the targets, pool saturation, job backlog, dead letters, stalled workers, unreadable queue metrics, mirror secret detection, mirror lag and held events, and PostgreSQL disk and WAL archiving (these need postgres_exporter, node_exporter or kubelet volume stats). Load them with `metrics.prometheusRule.enabled=true` or as a plain rule file.

Validation: `make check-alerts` runs `promtool check rules` and the unit tests in `deploy/observability/alerts-test.yaml` (which prove six alerts fire with the expected labels and text). Prometheus 3.5 loaded all 17 rules with health `ok` on the disposable cluster.

## Verified (2026-10-22, disposable cluster)

Jaeger received traces from all three services; a `memory_search` trace had 20 spans (HTTP, token lookup, tool, Ollama query embedding 1.76 s cold, 13 ms of database work). Prometheus scraped all four pods (2 api, worker, mirror). Earlier, `TestTelemetrySampleWorkload` checked metrics, spans and the audit `request_id` = trace id in the integration suite.
