# Deploying job-service to AWS ECS (Fargate) with the ADOT collector

This directory holds the **deployment-side** scaffolding to run `job-service`
on ECS Fargate with an [AWS Distro for OpenTelemetry](https://aws-otel.github.io/)
(ADOT) collector running as a **sidecar** in the same task.

```
deploy/
├── ecs/
│   └── task-definition.json     # app container + aws-otel-collector sidecar (Fargate)
├── otel/
│   └── collector-config.yaml     # collector pipeline: OTLP -> X-Ray (traces) + CloudWatch (metrics)
└── iam/
    ├── task-role-policy.json      # runtime identity: SQS + S3 + X-Ray + CloudWatch
    └── execution-role-policy.json # SSM read for the collector config
```

> **Note:** the app **is** OpenTelemetry-instrumented (`app/otel.go`, `app/main.go`):
> `otelhttp` on the job endpoints, `otelaws` spans for SQS/S3, a `processMessage`
> span, `jobs.created` / `job.processing.duration` instruments, SQS trace-context
> propagation, and `slog` logs carrying `trace_id`/`span_id`. It exports OTLP/gRPC
> to the collector sidecar configured here. See
> [ADR 0001](../docs/adr/0001-observability-stack.md) (accepted).

## How the sidecar pattern works

ECS has no daemonsets. On Fargate, the collector runs as a second container in
the **same task definition**. Containers in a task share the `awsvpc` network
namespace, so the app reaches the collector at `localhost:4317` (OTLP/gRPC).

```
┌──────────────────── ECS task (awsvpc) ────────────────────┐
│  job-service  ──OTLP localhost:4317──▶  aws-otel-collector │
│   :8080                                   │   │            │
└───────────────────────────────────────────│───│────────────┘
                                  X-Ray ◀────┘   └────▶ CloudWatch (EMF)
```

The app is wired to the collector purely through env vars in the task def
(`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317`, `OTEL_SERVICE_NAME`), so
no app code change is needed to point at the sidecar.

## Two IAM roles (don't confuse them)

ECS gives each task two roles:

| Role | Used by | Needs |
|---|---|---|
| **Task role** (`taskRoleArn`) | your app **and** the collector at runtime | SQS + S3 (app) and X-Ray + CloudWatch (collector export) — see `iam/task-role-policy.json` |
| **Execution role** (`executionRoleArn`) | the ECS agent at launch | pull images, write logs, **read the collector config from SSM** — managed policy + `iam/execution-role-policy.json` |

For the **execution role**, attach the AWS-managed `AmazonECSTaskExecutionRolePolicy`
(covers ECR pulls + CloudWatch Logs) **plus** the inline `execution-role-policy.json`
(adds the SSM read). The container images here come from Docker Hub and **public**
ECR, neither of which needs ECR auth — the managed policy is for log creation and
future private-ECR use.

## Deploy steps

All commands assume the AWS CLI is configured and you've replaced the
placeholders (`<ACCOUNT_ID>`, `<your-bucket-name>`, region, ARNs) in the JSON/YAML.

### 1. Create the IAM roles

```bash
# Trust policy so ECS tasks can assume the roles
cat > /tmp/ecs-trust.json <<'EOF'
{ "Version": "2012-10-17", "Statement": [
  { "Effect": "Allow", "Principal": { "Service": "ecs-tasks.amazonaws.com" }, "Action": "sts:AssumeRole" } ] }
EOF

# Task role (app + collector runtime perms)
aws iam create-role --role-name job-service-task-role \
  --assume-role-policy-document file:///tmp/ecs-trust.json
aws iam put-role-policy --role-name job-service-task-role \
  --policy-name job-service-task --policy-document file://iam/task-role-policy.json

# Execution role (image pull, logs, SSM config read)
aws iam create-role --role-name job-service-execution-role \
  --assume-role-policy-document file:///tmp/ecs-trust.json
aws iam attach-role-policy --role-name job-service-execution-role \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy
aws iam put-role-policy --role-name job-service-execution-role \
  --policy-name job-service-exec --policy-document file://iam/execution-role-policy.json
```

### 2. Upload the collector config to SSM Parameter Store

The task def injects this as the `AOT_CONFIG_CONTENT` secret, and the collector
starts with `--config=env:AOT_CONFIG_CONTENT`.

```bash
aws ssm put-parameter \
  --name /job-service/otel-collector-config \
  --type String \
  --value "$(cat otel/collector-config.yaml)" \
  --overwrite
```

### 3. Register the task definition

```bash
aws ecs register-task-definition --cli-input-json file://ecs/task-definition.json
```

### 4. Create / update the service

Needs an existing cluster, two subnets, and a security group allowing inbound
`8080` (front it with an ALB in real use).

```bash
aws ecs create-service \
  --cluster <your-cluster> \
  --service-name job-service \
  --task-definition job-service \
  --desired-count 1 \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[<subnet-a>,<subnet-b>],securityGroups=[<sg-id>],assignPublicIp=ENABLED}"
```

To roll out a new task def revision later: `aws ecs update-service --cluster <your-cluster> --service job-service --task-definition job-service --force-new-deployment`.

## Verifying

- **Collector health:** check the `otel-collector` log stream in the
  `/ecs/job-service` CloudWatch log group — it logs its enabled pipelines on start.
- **Traces:** once the app is instrumented, X-Ray console → Traces.
- **Metrics:** CloudWatch → Metrics → `job-service` namespace.

## EC2 launch type (alternative)

If you run ECS on EC2 instead of Fargate, you can keep the sidecar exactly as-is,
**or** run the collector once per instance as a separate ECS service with the
`daemon` scheduling strategy (closer to the EKS daemonset model). The app then
targets the host IP rather than `localhost`. The sidecar in this task def is the
simpler default and is the only option that also works on Fargate.
