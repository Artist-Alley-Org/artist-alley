# ADR 0033: Observability & operator telemetry — metrics, traces, log shipping

- Date: 2026-05-30
- Status: Accepted

## Context

Self-hosted ops teams cannot run a production service blind. The
current Artist Alley setup ships `/healthz` and `/readyz`, structured
JSON logs to stdout, and not much else. Operators need to:

- Pull metrics into Prometheus / VictoriaMetrics / equivalent for
  dashboards + alerting.
- Trace a slow request through the stack with OpenTelemetry, across
  HTTP → DB → job queue → external API hops.
- Ship structured logs to Loki / Datadog / Cloudwatch / wherever
  the studio's logging story lives.
- Tail recent logs from the admin UI without SSHing into the box —
  for debugging at 3 AM when the SRE is on vacation.
- Adjust per-subsystem log levels live without a restart.

The audit log (Phase 1.40, ADR 0032) is the **what happened**
substrate; observability is the **how is it performing** substrate.
Different consumers, different cardinality, different retention,
different export pipelines.

## Decision

Add Phase 1.41 — Observability & operator telemetry — as the
operator-facing production-readiness layer.

### Metrics — Prometheus format

`GET /metrics` exposes the standard Prometheus exposition format,
gated by an admin-configurable bearer token (default: enabled, token
required from a fresh install).

**Exposed metrics:**

| Family | Samples |
|---|---|
| HTTP | `http_requests_total{route,method,status}`, `http_request_duration_seconds{route}` |
| DB | `db_pool_connections{state}`, `db_query_duration_seconds{table,op}` |
| Job queue | `jobs_queued_total{type}`, `jobs_processing_duration_seconds{type}`, `jobs_failed_total{type,reason}` |
| Storage | `storage_op_duration_seconds{backend,op}`, `storage_bytes_total{backend,direction}` |
| Cache | `cache_hits_total{registry}`, `cache_misses_total{registry}`, `cache_size_bytes{registry}` |
| Sessions | `sessions_active`, `active_seats_30d` (the license-tier value) |
| License | `license_assets_total`, `license_quota_remaining{quota}` |
| Federation | `federation_peers{state}`, `federation_outbox_pending` |
| Audit | `audit_events_total{category}`, `audit_buffer_dropped_total` |

Custom metrics via the plugin SDK (Phase 1.23) — plugins register a
metric at boot and emit during operation; surfaces in `/metrics`
under a `plugin_` prefix.

### Tracing — OpenTelemetry

OTLP-format export to the operator's chosen collector. Configured via
admin:

- **Backend** — endpoint, headers, sampling rate.
- **Auto-instrumented:** HTTP handlers, DB queries (sqlc), job queue
  workers, external API calls (federation, platform integrations,
  AI providers, chat providers).
- **Custom spans** — code can wrap any block with
  `trace.Span(ctx, "name")` for finer granularity.
- **W3C Trace Context propagation** — incoming `traceparent`
  headers continue an upstream trace; outgoing requests carry their
  own headers.

Default sample rate is 0.1 (10 % of requests) to keep cardinality
in check. Errors are 100 % sampled regardless.

### Log shipping

Default: structured JSON to stdout (cloud-native standard, works with
any Docker / Kubernetes / systemd log pipeline).

**Optional forwarders** as opt-in providers:

| Provider | Wire format | Notes |
|---|---|---|
| **Loki** | HTTP `/loki/api/v1/push` | Grafana stack alignment |
| **Datadog** | HTTP `/v1/input` | Enterprise common |
| **Cloudwatch Logs** | AWS SDK | AWS-hosted operators |
| **Vector** | TCP / Unix socket | Studio-standard pipeline |
| **Promtail** | File scrape | Pull, not push |
| **Custom syslog** | UDP / TCP | Legacy SIEM compatibility |

Forwarders are isolated Go packages in `app/internal/observability/
forwarders/<name>/` following the provider pattern from ADRs 0021,
0022, 0026, 0030, 0031.

### Per-subsystem log levels

Admin surface lists every named subsystem (auth, audit, jobs,
storage, preview, federation, etc.) with a current log level. Changes
are hot-reloaded — no restart. The level set persists in
`system_config` and survives restarts.

A `?log_level=debug` query parameter on any HTTP request elevates
the log level for that single request only — for debugging in
production without affecting the whole system.

### Admin live-tail log viewer

`GET /admin/logs/tail` — WebSocket endpoint, admin role required.
Streams the last N seconds of structured logs with filter support
(level, subsystem, free-text search). Useful when the SRE doesn't
have shell access at the moment they need it.

### Health endpoints

- `/healthz` — liveness (the process is alive). Always 200 if the
  binary is responding.
- `/readyz` — readiness (DB connected, storage accessible, license
  valid, federation outbox not stalled). 200 / 503 only.
- `/admin/system/health` — per-subsystem detailed status with last
  check timestamp. Used by the homepage admin tile and by anyone
  running an external healthcheck script.

### Pre-baked Grafana dashboards

Ship `infra/grafana/dashboards/*.json` as starter dashboards:

- HTTP performance — RED metrics by route.
- Job queue health — queue depth, processing time, failure rate.
- Storage performance — read/write latency by backend.
- Federation health — peer connectivity, outbox depth.
- License metrics — active seats vs cap, asset count vs cap, quota
  remaining.
- Audit volume — events per category over time.

Operators import once and have a working ops view immediately.

### Per-tier behaviour

- **Community + Pro:** metrics + tracing + log shipping + live tail
  + Grafana dashboards. Same feature.
- **Enterprise:** + priority support for ops issues, + bring-your-own
  observability vendor support (New Relic, Honeycomb, etc. — pre-
  baked OTLP exporters with vendor-specific config tweaks), + SLA
  on resolution time for production incidents.

Enforced via the tangled-derivation model from ADR 0017 (Enterprise
exposes a `vendor_observability_enabled` derived value).

### What this is NOT

- **Not the audit log.** Audit (Phase 1.40, ADR 0032) is
  user-meaningful events; observability is performance + debugging
  telemetry.
- **Not the activity stream.** Activity is user-visible (Phase
  1.21); observability is operator-facing.

## Consequences

**Positive**

- Operators can run a real production deployment without
  third-party blind spots.
- The Prometheus + OpenTelemetry choice aligns with the broader
  cloud-native ecosystem — no vendor lock-in, drop-in for any
  major observability vendor.
- Pre-baked Grafana dashboards remove the "I have metrics but
  what should they look like" gap that kills observability
  adoption.
- Live-tail log viewer is a meaningful debugging primitive that
  doesn't require shell access — important for hosted Pro
  customers who don't have it.

**Negative**

- Cardinality discipline: route labels in HTTP metrics must be
  parameterized (`/assets/{id}` not `/assets/123`) or cardinality
  explodes. Mitigation: linter on metric label values + a
  high-cardinality alert in the pre-baked dashboards.
- Tracing adds a per-request cost. Mitigation: 10 % sampling
  default; error-always-sampled; operator can tune.

## Alternatives considered

- **Just structured logs; skip metrics + traces.** Insufficient for
  production. Rejected.
- **Vendor-specific (Datadog APM only, or New Relic only).**
  Vendor lock-in. The cloud-native OTLP + Prometheus standard is
  the correct on-ramp; vendor-specific exporters are wrappers on
  top of it.
- **Push metrics instead of pull.** Pull (Prometheus model) is the
  cloud-native default and matches operator expectations. A push
  variant (StatsD, etc.) is a follow-up provider.

## Reference

- Phase 1.41 in [`docs/roadmap.md`](../roadmap.md).
- Audit log (Phase 1.40, ADR 0032) is the separate substrate.
- Plugin SDK (Phase 1.23) — plugins register custom metrics.
- ADR 0017 monetization — Enterprise BYO-vendor + SLA.
