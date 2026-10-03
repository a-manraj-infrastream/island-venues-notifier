// Package mailer sends plain-text e-mails, either through SendGrid's v3 Web
// API or, when no credentials are configured, by logging what would be sent.
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Message is a single plain-text e-mail to one recipient.
type Message struct {
	To      string
	Subject string
	Body    string
}

const (
	// DefaultEndpoint is SendGrid's mail send API. It is the only egress the
	// project's network policy allows.
	DefaultEndpoint = "https://api.sendgrid.com/v3/mail/send"
	// DefaultTimeout bounds one send. Cloud Run gives a request up to the
	// service timeout, but Pub/Sub's push deadline is the real limit: a slow
	// send that outlives it is redelivered while still in flight.
	DefaultTimeout = 10 * time.Second
	// maxErrorBody caps how much of a SendGrid error response is kept in the
	// returned error (and therefore in logs).
	maxErrorBody = 1024
)

// SendGrid is a minimal client for POST /v3/mail/send.
type SendGrid struct {
	apiKey   string
	from     string
	fromName string
	endpoint string
	client   *http.Client
}

// SendGridOption customises a SendGrid client.
type SendGridOption func(*SendGrid)

// WithEndpoint overrides the API URL (used by tests).
func WithEndpoint(url string) SendGridOption {
	return func(s *SendGrid) { s.endpoint = url }
}

// WithHTTPClient replaces the HTTP client. Its Timeout should be set; the
// default client has none and a hung connection would block the delivery.
func WithHTTPClient(c *http.Client) SendGridOption {
	return func(s *SendGrid) { s.client = c }
}

// WithFromName sets the display name of the sender.
func WithFromName(name string) SendGridOption {
	return func(s *SendGrid) { s.fromName = name }
}

// NewSendGrid returns a client sending as from. apiKey and from are required.
func NewSendGrid(apiKey, from string, opts ...SendGridOption) (*SendGrid, error) {
	if apiKey == "" {
		return nil, errors.New("failed to create SendGrid client: API key is required")
	}
	if from == "" {
		return nil, errors.New("failed to create SendGrid client: sender e-mail is required")
	}
	s := &SendGrid{
		apiKey:   apiKey,
		from:     from,
		endpoint: DefaultEndpoint,
		client:   &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

type sgAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type sgPersonalization struct {
	To []sgAddress `json:"to"`
}

type sgContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type sgRequest struct {
	Personalizations []sgPersonalization `json:"personalizations"`
	From             sgAddress           `json:"from"`
	Subject          string              `json:"subject"`
	Content          []sgContent         `json:"content"`
}

// Send delivers msg. Any non-2xx answer from SendGrid is returned as an error
// so the caller can ask Pub/Sub to retry.
func (s *SendGrid) Send(ctx context.Context, msg Message) error {
	payload, err := json.Marshal(sgRequest{
		Personalizations: []sgPersonalization{{To: []sgAddress{{Email: msg.To}}}},
		From:             sgAddress{Email: s.from, Name: s.fromName},
		Subject:          msg.Subject,
		Content:          []sgContent{{Type: "text/plain", Value: msg.Body}},
	})
	if err != nil {
		return fmt.Errorf("failed to encode SendGrid request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build SendGrid request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call SendGrid: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("failed to send e-mail: SendGrid returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	// Drain so the keep-alive connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return nil
}

// Log is the fallback mailer used when SendGrid is not configured. It records
// the e-mail it would have sent and always succeeds.
type Log struct {
	Logger *slog.Logger
}

// Send logs msg at INFO level.
func (l Log) Send(ctx context.Context, msg Message) error {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "e-mail not sent: SendGrid is not configured",
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("body", msg.Body),
	)
	return nil
}
