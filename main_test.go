package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-manraj-infrastream/island-venues-notifier/internal/mailer"
	"google.golang.org/api/idtoken"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg := loadConfig(env(nil))
	if cfg.Port != "8080" || cfg.ServiceName != "island-venues-notifier" || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("defaults = %+v", cfg)
	}
	cfg = loadConfig(env(map[string]string{"RELEASE_VERSION": "1.2.3", "LOG_LEVEL": "debug", "PORT": "9000"}))
	if cfg.ServiceVersion != "1.2.3" || cfg.LogLevel != slog.LevelDebug || cfg.Port != "9000" {
		t.Errorf("overrides = %+v", cfg)
	}
	cfg = loadConfig(env(map[string]string{"RELEASE_VERSION": "1.2.3", "SERVICE_VERSION": "9.9.9"}))
	if cfg.ServiceVersion != "9.9.9" {
		t.Errorf("SERVICE_VERSION should win, got %q", cfg.ServiceVersion)
	}
	cfg = loadConfig(env(map[string]string{"INFRASTREAM_APP_VERSION": "v4.5.6"}))
	if cfg.ServiceVersion != "v4.5.6" {
		t.Errorf("INFRASTREAM_APP_VERSION fallback, got %q", cfg.ServiceVersion)
	}
}

// TestLoadConfigTrimsSecretWhitespace: a key uploaded with `echo` ends in a
// newline, which net/http refuses in a header value.
func TestLoadConfigTrimsSecretWhitespace(t *testing.T) {
	cfg := loadConfig(env(map[string]string{"SENDGRID_API_KEY": "SG.key\n", "SENDER_EMAIL": " bookings@example.mu\n"}))
	if cfg.SendGridAPIKey != "SG.key" || cfg.SenderEmail != "bookings@example.mu" {
		t.Errorf("credentials not trimmed: key=%q sender=%q", cfg.SendGridAPIKey, cfg.SenderEmail)
	}
}

func TestNewMailerSelection(t *testing.T) {
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name     string
		cfg      config
		sendGrid bool
	}{
		{"none", config{}, false},
		{"key only", config{SendGridAPIKey: "k"}, false},
		{"sender only", config{SenderEmail: "a@example.com"}, false},
		{"both", config{SendGridAPIKey: "k", SenderEmail: "a@example.com"}, true},
	}
	for _, c := range cases {
		m, err := newMailer(c.cfg, quiet)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, isSG := m.(*mailer.SendGrid)
		if isSG != c.sendGrid {
			t.Errorf("%s: SendGrid = %v, want %v", c.name, isSG, c.sendGrid)
		}
	}
}

func TestRouter(t *testing.T) {
	var pushed int
	router := newRouter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushed++
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/", http.StatusOK},
		{http.MethodGet, "/", http.StatusMethodNotAllowed},
		{http.MethodPost, "/other", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, bytes.NewReader(nil)))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
		if c.path == "/healthz" && strings.TrimSpace(rec.Body.String()) != "ok" {
			t.Errorf("healthz body = %q", rec.Body)
		}
	}
	if pushed != 1 {
		t.Errorf("push handler called %d times, want 1", pushed)
	}
}

// TestPushGuard: the service fails closed. Without a push identity it only
// starts in DEV_MODE, and never in DEV_MODE on Cloud Run.
func TestPushGuard(t *testing.T) {
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantOpen bool // an unauthenticated POST reaches the handler
	}{
		{"push identity set", map[string]string{"PUSH_SERVICE_ACCOUNT": "sa@p.iam.gserviceaccount.com"}, "", false},
		{"push identity on Cloud Run", map[string]string{"PUSH_SERVICE_ACCOUNT": "sa@p.iam.gserviceaccount.com", "K_SERVICE": "notifier"}, "", false},
		{"dev mode locally", map[string]string{"DEV_MODE": "true"}, "", true},
		{"dev mode on Cloud Run", map[string]string{"DEV_MODE": "true", "K_SERVICE": "notifier"}, "DEV_MODE=true is refused on Cloud Run", false},
		{"nothing configured", nil, "PUSH_SERVICE_ACCOUNT is not set", false},
		{"nothing configured on Cloud Run", map[string]string{"K_SERVICE": "notifier"}, "PUSH_SERVICE_ACCOUNT is not set", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guard, err := pushGuard(loadConfig(env(c.env)), rejectAll{}, quiet)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			reached := false
			h := guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true }))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
			if reached != c.wantOpen {
				t.Fatalf("unauthenticated push reached handler = %v, want %v (status %d)", reached, c.wantOpen, rec.Code)
			}
			if !c.wantOpen && rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestLoadConfigPushAudiences(t *testing.T) {
	cfg := loadConfig(env(map[string]string{"PUSH_AUDIENCE": " https://a.run.app , ,https://b.example ", "PUSH_SERVICE_ACCOUNT": "sa@p.iam.gserviceaccount.com\n"}))
	if len(cfg.PushAudiences) != 2 || cfg.PushAudiences[0] != "https://a.run.app" || cfg.PushAudiences[1] != "https://b.example" {
		t.Errorf("audiences = %q", cfg.PushAudiences)
	}
	if cfg.PushServiceAccount != "sa@p.iam.gserviceaccount.com" {
		t.Errorf("service account not trimmed: %q", cfg.PushServiceAccount)
	}
}

// rejectAll never validates a token.
type rejectAll struct{}

func (rejectAll) Validate(context.Context, string, string) (*idtoken.Payload, error) {
	return nil, errors.New("invalid")
}
