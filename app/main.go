// Package main implements a minimal Go microservice with SQS and S3 integration.
// It provides HTTP endpoints for job creation and retrieval, with an optional
// background worker for processing jobs asynchronously.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

const (
	addr = ":8080"

	// maxBodyBytes caps the size of an incoming request body to guard against
	// oversized or malicious payloads.
	maxBodyBytes = 1 << 20 // 1 MiB

	// awsOpTimeout bounds each individual AWS API call so a hung dependency
	// cannot block a request or the worker indefinitely.
	awsOpTimeout = 10 * time.Second

	// shutdownTimeout bounds graceful shutdown of the HTTP server.
	shutdownTimeout = 15 * time.Second
)

// App holds the application state and AWS service clients.
type App struct {
	sqsClient *sqs.Client // SQS client for sending and receiving messages
	s3Client  *s3.Client  // S3 client for storing job results
	sqsURL    string      // SQS queue URL
	s3Bucket  string      // S3 bucket name for storing job results
}

// JobRequest represents the request body for creating a new job.
type JobRequest struct {
	Text string `json:"text"` // Text to be processed
}

// JobMessage represents a message sent to SQS queue.
type JobMessage struct {
	ID   string `json:"id"`   // Unique job identifier
	Text string `json:"text"` // Text to be processed
}

// JobResult represents the processed job result stored in S3.
type JobResult struct {
	ID          string    `json:"id"`           // Unique job identifier
	Text        string    `json:"text"`         // Original text
	Output      string    `json:"output"`       // Processed output (uppercase text)
	ProcessedAt time.Time `json:"processed_at"` // Timestamp when job was processed
}

// main initializes the application, sets up AWS clients, registers HTTP handlers,
// and starts the HTTP server. If WORKER_ENABLED is set to "true", it also starts
// the background worker loop for processing jobs. The server shuts down gracefully
// on SIGINT/SIGTERM.
func main() {
	// Install the structured, trace-correlated JSON logger before anything logs.
	setupLogging()

	// Load AWS region from environment variable, default to us-east-1
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	// Load AWS configuration using default credential chain
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		slog.Error("failed to load AWS config", "error", err)
		os.Exit(1)
	}

	// Validate required environment variables
	sqsURL := os.Getenv("SQS_QUEUE_URL")
	if sqsURL == "" {
		slog.Error("SQS_QUEUE_URL environment variable is required")
		os.Exit(1)
	}

	s3Bucket := os.Getenv("S3_BUCKET")
	if s3Bucket == "" {
		slog.Error("S3_BUCKET environment variable is required")
		os.Exit(1)
	}

	// Initialize OpenTelemetry (traces + metrics), exporting via OTLP to the
	// ADOT collector sidecar. Non-fatal: if setup fails the service still runs
	// and telemetry falls back to no-ops.
	otelShutdown, err := setupOTel(context.Background())
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, continuing without telemetry", "error", err)
		otelShutdown = func(context.Context) error { return nil }
	}
	if err := initInstruments(); err != nil {
		slog.Warn("failed to initialize metric instruments", "error", err)
	}

	// Trace every AWS SDK call (SQS, S3). Must be appended before the clients are
	// constructed so they capture the middleware.
	otelaws.AppendMiddlewares(&cfg.APIOptions)

	// Initialize application with AWS clients
	app := &App{
		sqsClient: sqs.NewFromConfig(cfg),
		s3Client:  s3.NewFromConfig(cfg),
		sqsURL:    sqsURL,
		s3Bucket:  s3Bucket,
	}

	// Register HTTP handlers using method-based routing (Go 1.22+). The {id}
	// wildcard matches a single path segment, so nested paths do not leak
	// through, and unmatched methods automatically return 405.
	// Health/readiness probes are left untraced to keep span volume low; the job
	// endpoints are wrapped with otelhttp to emit server spans.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.healthz)
	mux.HandleFunc("GET /readyz", app.readyz)
	mux.Handle("POST /jobs", otelhttp.NewHandler(http.HandlerFunc(app.createJob), "createJob"))
	mux.Handle("GET /jobs/{id}", otelhttp.NewHandler(http.HandlerFunc(app.getJob), "getJob"))

	// Root context cancelled on SIGINT/SIGTERM, used to stop the worker loop
	// and trigger graceful HTTP shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start worker loop if enabled
	if os.Getenv("WORKER_ENABLED") == "true" {
		go app.workerLoop(ctx)
		slog.Info("worker enabled, starting background processing")
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run the server in the background so main can wait for a shutdown signal.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Wait for either a fatal server error or a shutdown signal.
	select {
	case err := <-serverErr:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	}

	// Graceful shutdown: stop accepting new connections and let in-flight
	// requests finish, bounded by shutdownTimeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}

	// Flush and stop telemetry exporters so buffered spans/metrics are not lost.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer flushCancel()
	if err := otelShutdown(flushCtx); err != nil {
		slog.Error("OpenTelemetry shutdown failed", "error", err)
	}
	slog.Info("server stopped")
}

// healthz handles GET /healthz requests.
// Returns 200 OK with "ok" response for health checks.
func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// readyz handles GET /readyz requests.
// Returns 200 OK with "ready" if AWS clients are initialized, otherwise 503.
func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	if a.sqsClient == nil || a.s3Client == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready"))
}

// createJob handles POST /jobs requests.
// Accepts JSON {"text":"..."}, generates a job ID, sends message to SQS,
// and returns the job ID with 201 Created status. The request body is capped
// at maxBodyBytes and the text field must be non-empty.
func (a *App) createJob(w http.ResponseWriter, r *http.Request) {
	// Cap the request body to guard against oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// Decode request body
	var req JobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate input
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	// Generate unique job ID
	jobID := uuid.New().String()
	message := JobMessage{
		ID:   jobID,
		Text: req.Text,
	}

	// Marshal message to JSON
	messageBody, err := json.Marshal(message)
	if err != nil {
		http.Error(w, "failed to encode message", http.StatusInternalServerError)
		return
	}

	// Send message to SQS queue, bounded by a per-request timeout.
	ctx, cancel := context.WithTimeout(r.Context(), awsOpTimeout)
	defer cancel()
	_, err = a.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(a.sqsURL),
		MessageBody: aws.String(string(messageBody)),
		// Carry the current trace context through the queue so the worker can
		// continue the same trace when it processes this job.
		MessageAttributes: otelSQSAttributes(ctx),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to send message", "error", err)
		http.Error(w, "failed to send message", http.StatusInternalServerError)
		return
	}
	jobsCreated.Add(ctx, 1)

	// Return job ID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": jobID})
}

// getJob handles GET /jobs/{id} requests.
// Retrieves job result from S3 and returns it as JSON.
// Returns 404 only when the object does not exist, 500 for other S3 errors,
// and 200 OK with the job result when found.
func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	// Extract job ID from the path wildcard.
	jobID := r.PathValue("id")
	if jobID == "" {
		http.Error(w, "job id required", http.StatusBadRequest)
		return
	}

	// Get job result from S3, bounded by a per-request timeout.
	ctx, cancel := context.WithTimeout(r.Context(), awsOpTimeout)
	defer cancel()
	key := fmt.Sprintf("jobs/%s.json", jobID)
	result, err := a.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.s3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Distinguish a genuine "not found" from infrastructure errors
		// (permissions, throttling, network) so callers are not misled.
		var noSuchKey *s3types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "failed to get object", "key", key, "error", err)
		http.Error(w, "failed to get job", http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	// Decode job result from JSON
	var jobResult JobResult
	if err := json.NewDecoder(result.Body).Decode(&jobResult); err != nil {
		http.Error(w, "failed to decode job", http.StatusInternalServerError)
		return
	}

	// Return job result as JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobResult)
}

// workerLoop runs continuously to process messages from SQS queue.
// Uses long polling (20 seconds) to receive messages, processes each message,
// stores result in S3, and deletes message from queue after successful processing.
// It stops when ctx is cancelled (e.g. on shutdown). The in-flight message is
// allowed to finish cleanly before returning.
// Only runs when WORKER_ENABLED environment variable is set to "true".
func (a *App) workerLoop(ctx context.Context) {
	for {
		// Stop promptly if shutdown was requested.
		if ctx.Err() != nil {
			slog.Info("worker stopping")
			return
		}

		// Receive message from SQS with long polling (20 seconds). The
		// cancellable context lets shutdown interrupt the long poll.
		result, err := a.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(a.sqsURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     20, // Long polling
			// Return custom attributes so the worker can recover the trace
			// context that createJob injected.
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("worker stopping")
				return
			}
			slog.Error("failed to receive message", "error", err)
			// Back off before retrying, but stay responsive to shutdown.
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// Process each received message. Use a background-derived context so
		// the in-flight message completes even if shutdown is in progress.
		for _, message := range result.Messages {
			// Continue the trace started in createJob, carried via SQS attributes.
			// A background-derived context keeps the in-flight message processing
			// even if shutdown is in progress.
			msgCtx := otelSQSContext(context.Background(), message.MessageAttributes)
			if err := a.processMessage(msgCtx, message); err != nil {
				slog.ErrorContext(msgCtx, "failed to process message", "error", err)
				continue
			}

			// Delete message from queue after successful processing.
			delCtx, cancel := context.WithTimeout(context.Background(), awsOpTimeout)
			_, err = a.sqsClient.DeleteMessage(delCtx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(a.sqsURL),
				ReceiptHandle: message.ReceiptHandle,
			})
			cancel()
			if err != nil {
				slog.ErrorContext(msgCtx, "failed to delete message", "error", err)
			}
		}
	}
}

// processMessage processes a single SQS message.
// Unmarshals the message, converts text to uppercase, creates a job result,
// and stores it in S3 at jobs/{id}.json.
// Returns an error if any step fails.
func (a *App) processMessage(ctx context.Context, message types.Message) (err error) {
	// Span continuing the job's trace; record processing duration on the way out
	// and mark the span failed on error.
	ctx, span := tracer.Start(ctx, "processMessage")
	defer span.End()
	start := time.Now()
	defer func() {
		jobProcessingDuration.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(attribute.Bool("error", err != nil)))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	// Unmarshal message body
	var jobMsg JobMessage
	if err := json.Unmarshal([]byte(*message.Body), &jobMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}
	span.SetAttributes(attribute.String("job.id", jobMsg.ID))

	// Process text: convert to uppercase
	output := strings.ToUpper(jobMsg.Text)

	// Create job result with processed output
	jobResult := JobResult{
		ID:          jobMsg.ID,
		Text:        jobMsg.Text,
		Output:      output,
		ProcessedAt: time.Now(),
	}

	// Marshal result to JSON
	resultBody, err := json.Marshal(jobResult)
	if err != nil {
		return fmt.Errorf("failed to marshal result: %w", err)
	}

	// Store result in S3, bounded by a per-operation timeout so a hung put
	// cannot stall the worker indefinitely. Derived from the span context so the
	// S3 call appears as a child span in the trace.
	putCtx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()
	key := fmt.Sprintf("jobs/%s.json", jobMsg.ID)
	_, err = a.s3Client.PutObject(putCtx, &s3.PutObjectInput{
		Bucket:      aws.String(a.s3Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(resultBody),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to put object: %w", err)
	}

	return nil
}
