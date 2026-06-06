# go-microservice — agent context

Shared facts (overview, tech stack, architecture, commands, endpoints, env vars,
running locally, Docker, deployment) live in the README and are imported below.
This file adds only the agent-only layer: conventions, guardrails, and gotchas.

@README.md

## Code Conventions

- Single package `main` in `app/`. All types (`App`, `JobRequest`, `JobMessage`, `JobResult`) and handlers live in `main.go`; OpenTelemetry setup, instruments, and SQS trace-context carriers live in `otel.go`. Keep it to these two files unless a change clearly warrants splitting.
- Handlers are methods on `*App`; routing uses method-based mux patterns (`GET /jobs/{id}`), so the mux returns `405` for the wrong verb and `r.PathValue` extracts path params.
- Errors: handlers `http.Error(...)` with an explicit status; worker/helpers wrap with `fmt.Errorf("...: %w", err)`. Logging via stdlib `log`.
- AWS calls run under bounded contexts: handlers derive from `r.Context()`, the worker from `context.Background()`, each with `awsOpTimeout` (10s); `ReceiveMessage` uses the cancelable root context so shutdown interrupts the long poll.
- Keep doc comments on exported types/functions — existing code documents every handler and struct field.
- No automated tests exist yet (`make test` finds none). `*_test.go` is excluded from the Docker build via `.dockerignore`.

## Gotchas & Known Issues

- **Worker and API share one process.** A deployment with `WORKER_ENABLED=true` both serves traffic and drains the queue. Scaling replicas multiplies workers on the same queue (fine for SQS, but be aware).
- **No DLQ / retry cap in code.** A message that always fails `processMessage` is logged and left in the queue; redelivery depends on the SQS queue's own redrive policy (configured outside this repo).
- **Worker processes one message at a time** (`MaxNumberOfMessages: 1`, no concurrency) — a bottleneck under load.
- **`readyz` is shallow.** It only checks the AWS clients are non-nil (they never are after construction); it does not verify SQS/S3 reachability, so it effectively always returns ready.
- **Observability is built (traces + metrics), but logging is not trace-correlated yet.** `app/otel.go` wires the OpenTelemetry SDK (OTLP/gRPC traces + metrics, X-Ray IDs/propagation, ECS resource detection); handlers use `otelhttp`, AWS calls use `otelaws`, the worker has a `processMessage` span, and there are `jobs.created` / `job.processing.duration` instruments. Telemetry exports to the ADOT collector sidecar (`deploy/`). **Still stdlib `log`** — no `slog`/`trace_id` correlation yet (see ADR 0001 "still open").
- **Telemetry export is non-fatal.** If `setupOTel` fails or the collector is unreachable, the app still serves — instruments fall back to no-ops and spans are dropped. Don't make startup depend on the collector.

### Recently fixed (do not reintroduce)

- **Graceful shutdown** — server runs via `http.Server` + `signal.NotifyContext` (SIGINT/SIGTERM) and `server.Shutdown` with a 15s bound; the worker loop stops on context cancel and finishes its in-flight message.
- **Server timeouts** — `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout` are set on the `http.Server`.
- **Per-operation AWS timeouts** — all `context.TODO()` replaced; handlers derive from `r.Context()` and the worker from `context.Background()`, each bounded by `awsOpTimeout` (10s). `ReceiveMessage` uses the cancelable root context so shutdown interrupts the long poll.
- **`getJob` 404 vs 500** — uses `errors.As(&s3types.NoSuchKey)`; only a missing object is `404`, other S3 errors are logged and return `500`.
- **`createJob` input hardening** — body capped at 1 MiB via `http.MaxBytesReader`; empty/whitespace `text` is rejected with `400`.
- **Routing** — method-based mux patterns (`GET /healthz`, `POST /jobs`, `GET /jobs/{id}`); `{id}` matches a single segment (no nested-path leak) and wrong methods return `405` automatically via `r.PathValue`.

## Git Workflow

- Default/main branch: `main`. Commit style in history: Conventional Commits (`feat:`, `docs:`).
- Create the new branch with `feat/<feature-name>` with conventional commits message.

## Git Rules (HARD CONSTRAINTS)

- Claude Code **MUST NOT** execute write git commands: `add`, `commit`, `push`, `stash`,
  `reset --hard`, `rebase`, `merge`, `tag`, `checkout -b`
- Claude Code **MAY** run read-only commands: `status`, `log`, `diff`, `branch` (list), `show`
- All commits must come from the user only
- If asked to commit → refuse and let the user do it
- Workflow: Claude edits files → user reviews → user commits

## Confidence Threshold Rules

- **≥95% confidence** — proceed immediately, no need to ask
- **80–94% confidence** — summarize the plan in 2-3 bullets before action, wait for confirmation
- **<80% confidence** — always ask first, do not assume
- If there isn't enough info to assess confidence → treat as <80%

## Do's and Don'ts

- **Do** keep the app to `main.go` + `otel.go` unless a change genuinely needs separation.
- **Do** keep doc comments on exported identifiers and struct fields.
- **Do** treat `SQS_QUEUE_URL` and `S3_BUCKET` as required — the app exits without them.
- **Don't** commit AWS credentials; `.gitignore` already excludes `credentials`, `*.pem`, `*.key`, `.aws/`.
- **Don't** make startup or request handling depend on the OTel collector — telemetry is best-effort (no-ops if export fails).
- **Don't** add CI assumptions; there is no pipeline in this repo.

## Last Updated

2026-06-06
