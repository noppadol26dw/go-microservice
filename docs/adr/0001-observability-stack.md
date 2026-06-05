# ADR 0001: Observability Stack

- **Status:** Proposed — deferred, not yet implemented
- **Date:** 2026-06-05
- **Supersedes:** the standalone notes formerly in `docs/ADOT.md` and `docs/LGTM.md` (folded into this ADR)

## Context

The service (HTTP API + SQS worker + S3) currently has **no instrumentation**
beyond stdlib `log` writing unstructured lines to stderr. There is no tracing,
no metrics, and no way to follow a single job across the
`HTTP → SQS → Worker → S3` pipeline. When a job is slow or silently fails, there
is nothing to look at.

Two approaches were evaluated. **Both** instrument the application with the
OpenTelemetry Go SDK and differ only in the *backend* the telemetry is shipped to.

## Decision Drivers

- End-to-end trace of a job across HTTP → SQS → Worker → S3 (incl. trace-context propagation through the SQS message).
- HTTP and worker metrics: request rate, latency, status codes, messages processed, errors.
- Structured, trace-correlated logs.
- Deployment target is EKS; operational cost and who runs the backend.
- Vendor lock-in (AWS-native) vs self-hosting (no lock-in, more to operate).

## Considered Options

1. **ADOT** — AWS Distro for OpenTelemetry → AWS X-Ray (traces) + CloudWatch (metrics/logs).
2. **LGTM** — Grafana stack: **L**oki (logs) / **G**rafana (viz) / **T**empo (traces) / **M**imir (metrics), via OTLP + Prometheus.

## Decision Outcome

**Deferred.** No observability is implemented yet; this ADR records the evaluation
so the eventual choice is a one-step decision rather than a re-investigation.

Tentative lean (not binding): **ADOT** if the service stays AWS/EKS-native and the
team wants the lowest operational burden (X-Ray + CloudWatch are managed). Choose
**LGTM** if observability needs to span multiple clouds/backends or a Grafana stack
already exists to plug into. Because the app is instrumented with vendor-neutral
OpenTelemetry in either case, switching backends later is mostly a collector/exporter
config change, not an app rewrite.

## Comparison

| Feature | ADOT (X-Ray + CloudWatch) | LGTM (OTLP + Prometheus) |
|---|---|---|
| Traces backend | X-Ray | Tempo |
| Metrics backend | CloudWatch | Mimir (Prometheus) |
| Logs backend | CloudWatch Logs | Loki |
| Visualization | CloudWatch / X-Ray console | Grafana |
| Hosting | Managed by AWS | Self-hosted (you operate it) |
| Lock-in | AWS-native | None |
| Best for | AWS/EKS-native services | Multi-cloud / existing Grafana stack |

> App instrumentation (the OpenTelemetry SDK code) is **identical** for both; only
> the exporter/collector configuration differs.

## Implementation sketch (for whichever option is chosen)

Shared, regardless of backend:

- Auto-instrument HTTP handlers with `otelhttp`:
  ```go
  mux.Handle("POST /jobs", otelhttp.NewHandler(http.HandlerFunc(app.createJob), "createJob"))
  ```
- Custom spans around the worker, propagating trace context through the SQS message body/attributes:
  ```go
  ctx, span := tracer.Start(ctx, "processMessage")
  defer span.End()
  span.SetAttributes(attribute.String("job.id", jobMsg.ID))
  ```
- Metrics: a `jobs_created_total` counter and a `job_processing_duration_seconds` histogram.
- Replace stdlib `log` with structured, trace-correlated logging (e.g. `slog`/`zerolog` carrying `trace_id`).

### Option 1 — ADOT specifics

- Deploy the **ADOT Collector** as a sidecar or daemonset in EKS.
- Export traces → X-Ray, metrics/logs → CloudWatch.
- Extra dep: `github.com/aws/aws-xray-sdk-go` (optional; OTLP→X-Ray works without it).

### Option 2 — LGTM specifics

- Export traces via OTLP gRPC to Tempo (`tempo:4317`).
- Expose a Prometheus `/metrics` endpoint for Mimir to scrape.
- Ship logs to Loki (direct client, or stdout + Promtail).
- Local stack via docker-compose: `tempo` + `mimir` + `loki` + `promtail` + `grafana`.

## Notes

- The dependency versions in the original notes were OpenTelemetry `v1.21.0` /
  contrib `v0.46.1` (late-2023). **Refresh to current releases at implementation
  time** — do not copy those pins.
- Until this is implemented, treat any observability reference in older docs as a
  proposal, not current behavior. The code in `app/main.go` has none of this.
