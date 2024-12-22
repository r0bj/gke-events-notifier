package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

const (
	ver string = "0.16"
)

var (
	verbose         = kingpin.Flag("verbose", "Verbose mode.").Short('v').Bool()
	port            = kingpin.Flag("port", "Port to listen on.").Envar("PORT").Default("8080").String()
	allowedTypeUrls = kingpin.Flag("allowed-type-urls", "Comma separated allowed type URLs. If empty, all types will be allowed.").Envar("ALLOWED_TYPE_URLS").String()
	slackWebhookUrl = kingpin.Flag("slack-webhook-url", "Slack webhook URL.").Envar("SLACK_WEBHOOK_URL").Required().String()
)

// PubSubMessage contains PubSub message content
type PubSubMessage struct {
	Message struct {
		Data       []byte `json:"data"`
		Attributes struct {
			ClusterLocation string `json:"cluster_location"`
			ClusterName     string `json:"cluster_name"`
			Payload         string `json:"payload"`
			ProjectId       string `json:"project_id"`
			TypeUrl         string `json:"type_url"`
		} `json:"attributes"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// SlackRequestBody contains Slack request body
type SlackRequestBody struct {
	Text        string                   `json:"text,omitempty"`
	Attachments []SlackMessageAttachment `json:"attachments"`
}

// SlackMessageAttachment contains slack message attachment data
type SlackMessageAttachment struct {
	Text     string                 `json:"text,omitempty"`
	Color    string                 `json:"color,omitempty"`
	MrkdwnIn []string               `json:"mrkdwn_in,omitempty"`
	Fields   []SlackAttachmentField `json:"fields"`
}

// SlackAttachmentField contains slack attachment field data
type SlackAttachmentField struct {
	Short bool   `json:"short"`
	Title string `json:"title"`
	Value string `json:"value"`
}

func handlePubSub(w http.ResponseWriter, r *http.Request) {
	var m PubSubMessage
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Data read failed", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &m); err != nil {
		slog.Error("Data unmarshal failed", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	slog.Debug("Request", "data", strings.ReplaceAll(string(body), " ", ""))

	data := string(m.Message.Data)
	if data == "" {
		slog.Warn("Received empty data payload, skipping.")
		return
	}

	if m.Message.Attributes.TypeUrl == "" {
		slog.Warn("No type_url in message attributes, skipping Slack notification.")
		return
	}

	if *allowedTypeUrls != "" {
		allowedTypeUrlsList := strings.Split(*allowedTypeUrls, ",")
		for i := range allowedTypeUrlsList {
			allowedTypeUrlsList[i] = strings.TrimSpace(allowedTypeUrlsList[i])
		}

		allowedTypeUrlFound := false
		for _, allowedTypeUrl := range allowedTypeUrlsList {
			if m.Message.Attributes.TypeUrl == allowedTypeUrl {
				slog.Debug("Received type_url present on allowed list", "type_url", m.Message.Attributes.TypeUrl)
				allowedTypeUrlFound = true
				break
			}
		}

		if !allowedTypeUrlFound {
			slog.Debug("Received type_url is not on allowed list, skipping", "type_url", m.Message.Attributes.TypeUrl, "allowed list", *allowedTypeUrls)
			return
		}
	}

	slackRequestBody := SlackRequestBody{
		Text: data,
		Attachments: []SlackMessageAttachment{
			SlackMessageAttachment{
				Fields: fillMessageFields(m),
			},
		},
	}

	slog.Info("Sending slack notification", "message", data)
	if err := sendSlackNotificationWithRetry(r.Context(), *slackWebhookUrl, slackRequestBody); err != nil {
		slog.Error("Sending slack message fail", "error", err)
		http.Error(w, "Failed to send Slack notification", http.StatusInternalServerError)
	}
}

func fillMessageFields(pubSubMessage PubSubMessage) []SlackAttachmentField {
	fields := []SlackAttachmentField{
		SlackAttachmentField{
			Title: "cluster name",
			Value: pubSubMessage.Message.Attributes.ClusterName,
			Short: true,
		},
		SlackAttachmentField{
			Title: "cluster location",
			Value: pubSubMessage.Message.Attributes.ClusterLocation,
			Short: true,
		},
		SlackAttachmentField{
			Title: "project number",
			Value: pubSubMessage.Message.Attributes.ProjectId,
			Short: true,
		},
	}

	typeUrl := strings.Split(pubSubMessage.Message.Attributes.TypeUrl, ".")
	eventType := typeUrl[len(typeUrl)-1]

	fields = append(fields, SlackAttachmentField{
		Title: "event type",
		Value: eventType,
		Short: true,
	})

	return fields
}

func sendSlackNotificationWithRetry(ctx context.Context, webhookUrl string, slackRequestBody SlackRequestBody) error {
	const maxAttempts = 3
	const baseDelay = time.Second

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Attempt to send
		lastErr = doSendSlackNotification(ctx, webhookUrl, slackRequestBody)
		if lastErr == nil {
			// Success on this attempt
			return nil
		}

		// If it's not the last attempt, wait before retrying
		if attempt < maxAttempts {
			// Log a warning that we're about to retry
			slog.Warn("Slack send failed, retrying...", "attempt", attempt, "error", lastErr)

			// Exponential backoff: for attempt n, wait 2^(n-1)*baseDelay
			delay := time.Duration(1<<(attempt-1)) * baseDelay
			select {
			case <-time.After(delay):
				// Continue to next attempt
			case <-ctx.Done():
				// If the context got canceled or timed out, stop retrying immediately
				return ctx.Err()
			}
		}
	}

	// All attempts failed
	return fmt.Errorf("Failed to send Slack notification after %d attempts: %w", maxAttempts, lastErr)
}

// doSendSlackNotification is your existing logic to send Slack messages.
func doSendSlackNotification(ctx context.Context, webhookUrl string, slackRequestBody SlackRequestBody) error {
	// Marshal the Slack request body
	slackBody, err := json.Marshal(slackRequestBody)
	if err != nil {
		return err
	}

	// Create the HTTP request using the provided context
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookUrl, bytes.NewBuffer(slackBody))
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 status returned from Slack: %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("failed to read Slack response body: %w", err)
	}

	if buf.String() != "ok" {
		return fmt.Errorf("non-ok response returned from Slack: %s", buf.String())
	}

	return nil
}

// handleHealthz responds with "OK" indicating the application is running.
func handleHealthz(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "OK\n")
}

// startHTTPServer starts the HTTP server to handle health and metrics endpoints.
func startHTTPServer(ctx context.Context, listenAddress string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/", handlePubSub)

	server := &http.Server{
		Addr:    listenAddress,
		Handler: mux,
	}

	// Shutdown the server gracefully when context is done
	go func() {
		<-ctx.Done()
		slog.Info("Shutting down HTTP server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Error shutting down HTTP server", "error", err)
		}
	}()

	slog.Info("Starting HTTP server", "address", listenAddress)

	return server.ListenAndServe()
}

func main() {
	var loggingLevel = new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: loggingLevel}))
	slog.SetDefault(logger)

	kingpin.Version(ver)
	kingpin.Parse()

	if *verbose {
		loggingLevel.Set(slog.LevelDebug)
	}

	slog.Info("Program started", "version", ver)

	listenAddress := fmt.Sprintf(":%s", *port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the HTTP server
	if err := startHTTPServer(ctx, listenAddress); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server encountered an error", "error", err)
		os.Exit(1)
	}

	slog.Info("Program gracefully stopped")
}
