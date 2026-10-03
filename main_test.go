package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-manraj-infrastream/island-venues-notifier/internal/mailer"
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
