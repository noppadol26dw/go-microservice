# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Install git for go modules
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY app/ ./app/

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o app ./app

# Final stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

# Copy binary from builder
COPY --from=builder /build/app /app

# Expose port
EXPOSE 8080

# Run as non-root user (distroless nonroot image already has this)
USER nonroot:nonroot

# Run the application
ENTRYPOINT ["/app"]

