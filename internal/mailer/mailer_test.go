package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSendGridSendsV3Request(t *testing.T) {
	var got struct {
		method, path, auth, contentType string
		body                            sgRequest
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusAccepted) // SendGrid answers 202 with no body
	}))
	defer srv.Close()

	sg, err := NewSendGrid("SG.test-key", "bookings@example.com",
		WithEndpoint(srv.URL+"/v3/mail/send"), WithFromName("Island Venues"))
	if err != nil {
		t.Fatal(err)
	}
	err = sg.Send(context.Background(), Message{To: "guest@example.com", Subject: "Hi", Body: "Line 1\nLine 2"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.method != http.MethodPost || got.path != "/v3/mail/send" {
		t.Errorf("request = %s %s, want POST /v3/mail/send", got.method, got.path)
	}
	if got.auth != "Bearer SG.test-key" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	b := got.body
	if len(b.Personalizations) != 1 || len(b.Personalizations[0].To) != 1 || b.Personalizations[0].To[0].Email != "guest@example.com" {
		t.Errorf("personalizations = %+v", b.Personalizations)
	}
	if b.From.Email != "bookings@example.com" || b.From.Name != "Island Venues" {
		t.Errorf("from = %+v", b.From)
	}
	if b.Subject != "Hi" {
		t.Errorf("subject = %q", b.Subject)
	}
	if len(b.Content) != 1 || b.Content[0].Type != "text/plain" || b.Content[0].Value != "Line 1\nLine 2" {
		t.Errorf("content = %+v", b.Content)
	}
}

func TestSendGridDefaultEndpoint(t *testing.T) {
	sg, err := NewSendGrid("k", "f@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if sg.endpoint != "https://api.sendgrid.com/v3/mail/send" {
		t.Errorf("endpoint = %q", sg.endpoint)
	}
	if sg.client.Timeout != DefaultTimeout || DefaultTimeout <= 0 {
		t.Errorf("default client timeout = %v, want %v", sg.client.Timeout, DefaultTimeout)
	}
}

func TestSendGridErrorStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"message":"nope"}]}` + strings.Repeat("x", 4096)))
		}))
		sg, _ := NewSendGrid("SG.secret-key", "f@example.com", WithEndpoint(srv.URL))
		err := sg.Send(context.Background(), Message{To: "a@example.com"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: want error", status)
		}
		msg := err.Error()
		if !strings.Contains(msg, "nope") || !strings.Contains(msg, strconv.Itoa(status)) {
			t.Errorf("status %d: error %q lacks SendGrid detail", status, msg)
		}
		if strings.Contains(msg, "SG.secret-key") {
			t.Errorf("status %d: error leaks the API key", status)
		}
		if len(msg) > maxErrorBody+200 {
			t.Errorf("status %d: error not bounded (%d bytes)", status, len(msg))
		}
	}
}

func TestSendGridTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	defer close(release)

	sg, _ := NewSendGrid("k", "f@example.com", WithEndpoint(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	start := time.Now()
	err := sg.Send(context.Background(), Message{To: "a@example.com"})
	if err == nil {
		t.Fatal("want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Send took %v, timeout not enforced", elapsed)
	}
}

func TestSendGridHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	sg, _ := NewSendGrid("k", "f@example.com", WithEndpoint(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sg.Send(ctx, Message{To: "a@example.com"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestNewSendGridRequiresCredentials(t *testing.T) {
	if _, err := NewSendGrid("", "f@example.com"); err == nil {
		t.Error("missing API key accepted")
	}
	if _, err := NewSendGrid("k", ""); err == nil {
		t.Error("missing sender accepted")
	}
}

func TestLogMailerLogsMessage(t *testing.T) {
	var buf bytes.Buffer
	l := Log{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	if err := l.Send(context.Background(), Message{To: "a@example.com", Subject: "S", Body: "B"}); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if rec["to"] != "a@example.com" || rec["subject"] != "S" || rec["body"] != "B" {
		t.Errorf("log record = %v", rec)
	}
}
