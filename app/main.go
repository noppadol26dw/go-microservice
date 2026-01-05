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

type App struct {
	sqsClient *sqs.Client
	s3Client  *s3.Client
	sqsURL    string
	s3Bucket  string
}

type JobRequest struct {
	Text string `json:"text"`
}

type JobMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type JobResult struct {
	ID          string    `json:"id"`
	Text        string    `json:"text"`
	Output      string    `json:"output"`
	ProcessedAt time.Time `json:"processed_at"`
}

func main() {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	sqsURL := os.Getenv("SQS_QUEUE_URL")
	if sqsURL == "" {
		log.Fatal("SQS_QUEUE_URL environment variable is required")
	}

	s3Bucket := os.Getenv("S3_BUCKET")
	if s3Bucket == "" {
		log.Fatal("S3_BUCKET environment variable is required")
	}

	app := &App{
		sqsClient: sqs.NewFromConfig(cfg),
		s3Client:  s3.NewFromConfig(cfg),
		sqsURL:    sqsURL,
		s3Bucket:  s3Bucket,
	}

	http.HandleFunc("/healthz", app.healthz)
	http.HandleFunc("/readyz", app.readyz)
	http.HandleFunc("/jobs", app.createJob)
	http.HandleFunc("/jobs/", app.getJob)

	workerEnabled := os.Getenv("WORKER_ENABLED") == "true"
	if workerEnabled {
		go app.workerLoop()
		log.Println("Worker enabled, starting background processing")
	}

	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

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

func (a *App) createJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req JobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	jobID := uuid.New().String()
	message := JobMessage{
		ID:   jobID,
		Text: req.Text,
	}

	messageBody, err := json.Marshal(message)
	if err != nil {
		http.Error(w, "failed to encode message", http.StatusInternalServerError)
		return
	}

	_, err = a.sqsClient.SendMessage(context.TODO(), &sqs.SendMessageInput{
		QueueUrl:    aws.String(a.sqsURL),
		MessageBody: aws.String(string(messageBody)),
	})
	if err != nil {
		log.Printf("failed to send message: %v", err)
		http.Error(w, "failed to send message", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": jobID})
}

func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := r.URL.Path[len("/jobs/"):]
	if jobID == "" {
		http.Error(w, "job id required", http.StatusBadRequest)
		return
	}

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

	var jobResult JobResult
	if err := json.NewDecoder(result.Body).Decode(&jobResult); err != nil {
		http.Error(w, "failed to decode job", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobResult)
}

func (a *App) workerLoop() {
	for {
		result, err := a.sqsClient.ReceiveMessage(context.TODO(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(a.sqsURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			log.Printf("failed to receive message: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, message := range result.Messages {
			if err := a.processMessage(message); err != nil {
				log.Printf("failed to process message: %v", err)
				continue
			}

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

func (a *App) processMessage(message types.Message) error {
	var jobMsg JobMessage
	if err := json.Unmarshal([]byte(*message.Body), &jobMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	output := strings.ToUpper(jobMsg.Text)

	jobResult := JobResult{
		ID:          jobMsg.ID,
		Text:        jobMsg.Text,
		Output:      output,
		ProcessedAt: time.Now(),
	}

	resultBody, err := json.Marshal(jobResult)
	if err != nil {
		return fmt.Errorf("failed to marshal result: %w", err)
	}

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
