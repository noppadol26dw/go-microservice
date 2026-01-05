# Go Microservice

Minimal Go microservice with SQS and S3 integration.

## Environment Variables

- `AWS_REGION` - AWS region (default: us-east-1)
- `SQS_QUEUE_URL` - SQS queue URL (required)
- `S3_BUCKET` - S3 bucket name (required)
- `WORKER_ENABLED` - Enable worker loop when set to "true"

## Running Locally

### Option 1: Using SSO Cached Credentials (Recommended)

If you use AWS SSO, export credentials from cached SSO session:

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

Build Docker image for local use:

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