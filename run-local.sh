#!/bin/bash
# Export AWS credentials from cached SSO credentials
eval $(aws configure export-credentials --format env --profile default)

# Set other required environment variables
export AWS_REGION=us-east-1
export SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/my-test-queue #Replace with your SQS queue URL
export S3_BUCKET=my-test-bucket #Replace with your S3 bucket name
export WORKER_ENABLED=true

# Run the application
make run