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