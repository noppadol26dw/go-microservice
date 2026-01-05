// Package main implements a minimal Go microservice with SQS and S3 integration.
// It provides HTTP endpoints for job creation and retrieval, with an optional
// background worker for processing jobs asynchronously.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
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
// the background worker loop for processing jobs.
func main() {
	// Load AWS region from environment variable, default to us-east-1
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	// Load AWS configuration using default credential chain
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	// Validate required environment variables
	sqsURL := os.Getenv("SQS_QUEUE_URL")
	if sqsURL == "" {
		log.Fatal("SQS_QUEUE_URL environment variable is required")
	}

	s3Bucket := os.Getenv("S3_BUCKET")
	if s3Bucket == "" {
		log.Fatal("S3_BUCKET environment variable is required")
	}

	// Initialize application with AWS clients
	app := &App{
		sqsClient: sqs.NewFromConfig(cfg),
		s3Client:  s3.NewFromConfig(cfg),
		sqsURL:    sqsURL,
		s3Bucket:  s3Bucket,
	}

	// Register HTTP handlers
	http.HandleFunc("/healthz", app.healthz)
	http.HandleFunc("/readyz", app.readyz)
	http.HandleFunc("/jobs", app.createJob)
	http.HandleFunc("/jobs/", app.getJob)

	// Start worker loop if enabled
	workerEnabled := os.Getenv("WORKER_ENABLED") == "true"
	if workerEnabled {
		go app.workerLoop()
		log.Println("Worker enabled, starting background processing")
	}

	// Start HTTP server
	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// healthz handles GET /healthz requests.
// Returns 200 OK with "ok" response for health checks.
func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// readyz handles GET /readyz requests.
// Returns 200 OK with "ready" if AWS clients are initialized, otherwise 503.
func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.sqsClient == nil || a.s3Client == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready"))
}

// createJob handles POST /jobs requests.
// Accepts JSON {"text":"..."}, generates a job ID, sends message to SQS,
// and returns the job ID with 201 Created status.
func (a *App) createJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Decode request body
	var req JobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
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

	// Send message to SQS queue
	_, err = a.sqsClient.SendMessage(context.TODO(), &sqs.SendMessageInput{
		QueueUrl:    aws.String(a.sqsURL),
		MessageBody: aws.String(string(messageBody)),
	})
	if err != nil {
		log.Printf("failed to send message: %v", err)
		http.Error(w, "failed to send message", http.StatusInternalServerError)
		return
	}

	// Return job ID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": jobID})
}

// getJob handles GET /jobs/{id} requests.
// Retrieves job result from S3 and returns it as JSON.
// Returns 404 if job not found, 200 OK with job result if found.
func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from URL path
	jobID := r.URL.Path[len("/jobs/"):]
	if jobID == "" {
		http.Error(w, "job id required", http.StatusBadRequest)
		return
	}

	// Get job result from S3
	key := fmt.Sprintf("jobs/%s.json", jobID)
	result, err := a.s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(a.s3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
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
// Only runs when WORKER_ENABLED environment variable is set to "true".
func (a *App) workerLoop() {
	for {
		// Receive message from SQS with long polling (20 seconds)
		result, err := a.sqsClient.ReceiveMessage(context.TODO(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(a.sqsURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     20, // Long polling
		})
		if err != nil {
			log.Printf("failed to receive message: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Process each received message
		for _, message := range result.Messages {
			if err := a.processMessage(message); err != nil {
				log.Printf("failed to process message: %v", err)
				continue
			}

			// Delete message from queue after successful processing
			_, err = a.sqsClient.DeleteMessage(context.TODO(), &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(a.sqsURL),
				ReceiptHandle: message.ReceiptHandle,
			})
			if err != nil {
				log.Printf("failed to delete message: %v", err)
			}
		}
	}
}

// processMessage processes a single SQS message.
// Unmarshals the message, converts text to uppercase, creates a job result,
// and stores it in S3 at jobs/{id}.json.
// Returns an error if any step fails.
func (a *App) processMessage(message types.Message) error {
	// Unmarshal message body
	var jobMsg JobMessage
	if err := json.Unmarshal([]byte(*message.Body), &jobMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

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

	// Store result in S3
	key := fmt.Sprintf("jobs/%s.json", jobMsg.ID)
	_, err = a.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
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
