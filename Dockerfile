# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install git for go modules
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY app/ ./app/

# Build static binary. Output to /build/bin/app so the target does not collide
# with the ./app source directory (which would make Go write the binary inside it).
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /build/bin/app ./app

# Final stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

# Copy binary from builder
COPY --from=builder /build/bin/app /app

# Expose port
EXPOSE 8080

# Run as non-root user (distroless nonroot image already has this)
USER nonroot:nonroot

# Run the application
ENTRYPOINT ["/app"]

