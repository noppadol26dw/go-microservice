# ADR 0001: Observability Stack

- **Status:** Accepted — app instrumented; **ADOT** backend chosen, ECS deployment scaffolded
- **Date:** 2026-06-05 (updated 2026-06-06)
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
- Deployment target is AWS ECS/EKS; operational cost and who runs the backend.
- Vendor lock-in (AWS-native) vs self-hosting (no lock-in, more to operate).

## Considered Options

1. **ADOT** — AWS Distro for OpenTelemetry → AWS X-Ray (traces) + CloudWatch (metrics/logs).
2. **LGTM** — Grafana stack: **L**oki (logs) / **G**rafana (viz) / **T**empo (traces) / **M**imir (metrics), via OTLP + Prometheus.

## Decision Outcome

**ADOT, on ECS.** The app is now instrumented with the vendor-neutral
OpenTelemetry Go SDK (traces + metrics over OTLP/gRPC, X-Ray-compatible trace IDs
and propagation, trace context carried across the SQS hop) — see `app/otel.go` and
`app/main.go`. Telemetry is exported to an **ADOT collector sidecar** that ships
traces → X-Ray and metrics → CloudWatch; the ECS Fargate deployment is scaffolded
in [`deploy/`](../../deploy/README.md).

ADOT was chosen over LGTM because the service is AWS/ECS-native and the team wants
the lowest operational burden (X-Ray + CloudWatch are managed). Should observability
later need to span multiple clouds/backends, **LGTM** remains viable: because the app
emits vendor-neutral OpenTelemetry, switching backends is mostly a collector/exporter
config change, not an app rewrite.

### Implemented

- OTLP traces + metrics, `otelhttp` on the job endpoints, `otelaws` spans for
  SQS/S3, a `processMessage` span, `jobs.created` counter and
  `job.processing.duration` histogram, SQS trace-context propagation.
- Structured, trace-correlated logging: stdlib `log` was replaced with `log/slog`
  (JSON handler) wrapped to add `trace_id` (X-Ray format) and `span_id` from the
  active span — see `setupLogging`/`traceHandler` in `app/otel.go`. Use the
  `slog.*Context(ctx, …)` variants so the span context reaches the handler.

All three decision drivers (traces, metrics, trace-correlated logs) are now met.

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

- Deploy the **ADOT Collector** alongside the service; topology depends on the
  compute platform:
  - **EKS** — collector as a sidecar or a daemonset.
  - **ECS Fargate** — collector as a **sidecar** container in the same task
    definition; the app reaches it at `localhost:4317` (containers in a task
    share the `awsvpc` network namespace).
  - **ECS on EC2** — sidecar (as above) or one collector per instance via an
    ECS service using the `daemon` scheduling strategy.
- Export traces → X-Ray, metrics/logs → CloudWatch.
- Extra dep: `github.com/aws/aws-xray-sdk-go` (optional; OTLP→X-Ray works without it).
- A worked ECS Fargate deployment (task definition, collector config, IAM) lives
  in [`deploy/`](../../deploy/README.md). The app SDK instrumentation is
  implemented (see "Decision Outcome").

### Option 2 — LGTM specifics

- Export traces via OTLP gRPC to Tempo (`tempo:4317`).
- Expose a Prometheus `/metrics` endpoint for Mimir to scrape.
- Ship logs to Loki (direct client, or stdout + Promtail).
- Local stack via docker-compose: `tempo` + `mimir` + `loki` + `promtail` + `grafana`.

## Notes

- Implemented against OpenTelemetry Go SDK **v1.44.0** (which requires Go ≥1.25;
  the repo builds on Go 1.26). The original late-2023 pins (`v1.21.0` / contrib
  `v0.46.1`) are historical only — see `go.mod` for the actual versions.
- The instrumentation lives in `app/otel.go` (setup, instruments, `slog` handler,
  SQS trace carriers) and `app/main.go` (handler/worker wiring). The logging
  backend is OTLP-agnostic `slog`; only the collector/exporter config is
  backend-specific.
