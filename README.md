# Go Microservice

Minimal Go HTTP microservice that accepts text jobs over HTTP, queues them via AWS SQS, processes them in a background worker, and stores results in AWS S3.

## Overview

- Job pipeline: `POST /jobs` → SQS → worker loop → uppercase the text → store result in S3 → `GET /jobs/{id}` reads it back.
- A single binary serves HTTP and (optionally) runs the worker loop in-process when `WORKER_ENABLED=true`.
- Published as a container image `noppadol26dw/job-service` on Docker Hub.

## Tech Stack

| Component | Version / Detail |
|---|---|
| Language | Go 1.25 (`go.mod`) |
| HTTP | stdlib `net/http` (no framework), listens on `:8080` |
| AWS SDK | `aws-sdk-go-v2` — `config`, `service/s3`, `service/sqs` |
| Observability | OpenTelemetry SDK — OTLP/gRPC traces + metrics, X-Ray propagation, `slog` JSON logs carrying `trace_id`/`span_id` (exports to the ADOT collector sidecar) |
| IDs | `github.com/google/uuid` |
| Container base | `golang:1.25-alpine` (build) → `gcr.io/distroless/static-debian12:nonroot` (runtime) |

## Architecture

```
Client ──POST /jobs──▶ HTTP handler ──SendMessage──▶ SQS queue
                                                        │
                                          ReceiveMessage (20s long poll)
                                                        ▼
Client ◀──GET /jobs/{id}── HTTP handler ◀──GetObject── S3 ◀──PutObject── worker loop
```

- The HTTP server and the worker loop run in the same process. The worker is a goroutine started only when `WORKER_ENABLED=true`; without it, the service only enqueues and serves reads.
- `processMessage` uppercases the job `text` and writes the `JobResult` JSON to S3 key `jobs/{id}.json`.
- The worker deletes the SQS message only after a successful S3 put; failures are logged and the message is left for redelivery.

## Directory Structure

```
.
├── app/
│   └── main.go        # entire application: App struct, HTTP handlers, worker loop
├── docs/
│   ├── adr/
│   │   └── 0001-observability-stack.md  # ADR: ADOT vs LGTM observability (proposed, deferred)
│   └── SEQUENCE.md    # mermaid sequence diagrams of the job/health/worker flows
├── Dockerfile         # multi-stage build → distroless nonroot image
├── Makefile           # build / test / run targets
└── run-local.sh       # exports SSO creds + env vars, then `make run`
```

## Commands

```bash
make build            # = go build -o bin/app ./app
make test             # = go test ./...
make run              # build, then ./bin/app (needs AWS creds + env vars)
./run-local.sh        # export SSO creds + env vars, then make run
```

## HTTP Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness — always `200 ok` |
| GET | `/readyz` | Readiness — `200 ready` if AWS clients initialized, else `503` |
| POST | `/jobs` | Body `{"text":"..."}` (≤1 MiB, non-empty) → `201 {"id":"<uuid>"}`; `400` on invalid/empty body |
| GET | `/jobs/{id}` | → `200` result JSON, `404` if missing, `500` on other S3 errors |

```bash
# Smoke test once running on :8080
curl -s localhost:8080/healthz
curl -s -XPOST localhost:8080/jobs -d '{"text":"hello"}'
curl -s localhost:8080/jobs/<id-from-previous>
```

## Environment Variables

| Variable | Required | Default | Notes |
|---|---|---|---|
| `AWS_REGION` | no | `us-east-1` | Passed to AWS config |
| `SQS_QUEUE_URL` | **yes** | — | Service exits on startup if unset |
| `S3_BUCKET` | **yes** | — | Service exits on startup if unset |
| `WORKER_ENABLED` | no | unset | Worker loop runs only when exactly `"true"` |

AWS credentials use the default credential chain (`config.LoadDefaultConfig`). No `.env` file is loaded by the app — export env vars in the shell or pass them to the container.

## Running Locally

### Option 1: Using SSO Cached Credentials (Recommended)

If you use AWS SSO, export credentials from the cached SSO session:

```bash
# Export credentials from AWS CLI cached SSO session
eval $(aws configure export-credentials --format env --profile default)

# Set other environment variables
export AWS_REGION=us-east-1
export SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/queue-name
export S3_BUCKET=your-bucket-name
export WORKER_ENABLED=true

# Build and run
make build
make run
```

Or use the provided script:
```bash
./run-local.sh
```

### Option 2: Using AWS Profile (if credentials are in ~/.aws/credentials)

```bash
export AWS_PROFILE=default
export AWS_REGION=us-east-1
export SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/queue-name
export S3_BUCKET=your-bucket-name
export WORKER_ENABLED=true

make build
make run
```

**Note:** If `~/.aws/credentials` is empty but AWS CLI works (using SSO), use Option 1.

## Docker

### Build Locally

```bash
docker build -t job-service:local .
```

Run the container locally:

```bash
docker run -p 8080:8080 \
  -e AWS_REGION=us-east-1 \
  -e SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/queue-name \
  -e S3_BUCKET=your-bucket-name \
  -e WORKER_ENABLED=true \
  job-service:local
```

### Build and Push to Docker Hub

```bash
# Build image
docker build -t noppadol26dw/job-service:v1 .

# Push to Docker Hub
docker push noppadol26dw/job-service:v1
```

**Docker Hub:** https://hub.docker.com/r/noppadol26dw/job-service

## Deployment

- No CI/CD pipeline is configured in this repo (no `.github/workflows`, no `Jenkinsfile`).
- Release is manual: `docker build -t noppadol26dw/job-service:vX . && docker push noppadol26dw/job-service:vX`.
