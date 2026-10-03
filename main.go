// Command island-venues-notifier receives booking-created events from
// Eventarc (Pub/Sub push) and e-mails a confirmation to the guest.
//
// CI builds this file on its own (`go build main.go`), so package main must
// stay in this single file; everything else lives under internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/a-manraj-infrastream/island-venues-notifier/internal/delivery"
	"github.com/a-manraj-infrastream/island-venues-notifier/internal/mailer"
	"github.com/a-manraj-infrastream/island-venues-notifier/internal/pushauth"
	"github.com/a-manraj-infrastream/island-venues-notifier/internal/telemetry"
	"google.golang.org/api/idtoken"
)

const (
	defaultPort        = "8080"
	defaultServiceName = "island-venues-notifier"
	senderName         = "Island Venues"
	// Cloud Run sends SIGTERM and kills the container 10 s later. Drain
	// requests for most of that window and keep the rest for the trace flush.
	shutdownGrace = 8 * time.Second
	flushGrace    = 2 * time.Second
)

// config is everything the service reads from its environment.
type config struct {
	Port           string
	ProjectID      string
	ServiceName    string
	ServiceVersion string
	SendGridAPIKey string
	SenderEmail    string
	LogLevel       slog.Level
	// PushServiceAccount is the Eventarc trigger's service account; only
	// pushes carrying a Google OIDC token for it are accepted.
	PushServiceAccount string
	// PushAudiences are the accepted token audiences (comma-separated in
	// PUSH_AUDIENCE). Empty means "https://" + the request Host.
	PushAudiences []string
	// DevMode accepts unauthenticated pushes. Local runs only.
	DevMode bool
	// OnCloudRun is true when K_SERVICE is set by Cloud Run.
	OnCloudRun bool
}

func loadConfig(getenv func(string) string) config {
	cfg := config{
		Port:           getenv("PORT"),
		ProjectID:      getenv("GOOGLE_CLOUD_PROJECT"),
		ServiceName:    getenv("SERVICE_NAME"),
		ServiceVersion: getenv("SERVICE_VERSION"),
		// Secret Manager values often end with the newline `echo` added when
		// they were uploaded. A newline in the Authorization header makes
		// net/http refuse every SendGrid request, so each push would fail
		// with 5xx and be redelivered forever. Neither value can legitimately
		// contain surrounding whitespace.
		SendGridAPIKey: strings.TrimSpace(getenv("SENDGRID_API_KEY")),
		SenderEmail:    strings.TrimSpace(getenv("SENDER_EMAIL")),
		// Same trailing-newline risk as the secrets above: a stray newline
		// would make every e-mail comparison fail and reject every push.
		PushServiceAccount: strings.TrimSpace(getenv("PUSH_SERVICE_ACCOUNT")),
		DevMode:            getenv("DEV_MODE") == "true",
		OnCloudRun:         getenv("K_SERVICE") != "",
	}
	for _, aud := range strings.Split(getenv("PUSH_AUDIENCE"), ",") {
		if aud = strings.TrimSpace(aud); aud != "" {
			cfg.PushAudiences = append(cfg.PushAudiences, aud)
		}
	}
	if cfg.Port == "" {
		cfg.Port = defaultPort
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = defaultServiceName
	}
	if cfg.ServiceVersion == "" {
		// The engine-generated image exports RELEASE_VERSION; use it so
		// traces and profiles carry the deployed version without extra config.
		cfg.ServiceVersion = getenv("RELEASE_VERSION")
	}
	if cfg.ServiceVersion == "" {
		// Set by the engine on the Cloud Run service (the deployed version).
		cfg.ServiceVersion = getenv("INFRASTREAM_APP_VERSION")
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL"))); err != nil {
		cfg.LogLevel = slog.LevelInfo
	}
	return cfg
}

// newMailer picks SendGrid when both credentials are present, the log-only
// mailer otherwise. Half a configuration is reported loudly instead of being
// silently treated as "local mode".
func newMailer(cfg config, logger *slog.Logger) (delivery.Mailer, error) {
	if cfg.SendGridAPIKey == "" || cfg.SenderEmail == "" {
		if cfg.SendGridAPIKey != "" || cfg.SenderEmail != "" {
			logger.Warn("SENDGRID_API_KEY and SENDER_EMAIL must both be set; e-mails will only be logged")
		} else {
			logger.Info("SendGrid not configured; e-mails will only be logged")
		}
		return mailer.Log{Logger: logger}, nil
	}
	sg, err := mailer.NewSendGrid(cfg.SendGridAPIKey, cfg.SenderEmail,
		mailer.WithFromName(senderName),
		mailer.WithHTTPClient(&http.Client{
			Timeout:   mailer.DefaultTimeout,
			Transport: telemetry.ThirdPartyTransport(http.DefaultTransport),
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to configure SendGrid: %w", err)
	}
	return sg, nil
}

// pushGuard decides how pushes are authenticated, failing closed.
//   - PUSH_SERVICE_ACCOUNT set: only verified Eventarc OIDC tokens get in.
//   - DEV_MODE=true and not on Cloud Run: no check (local development).
//   - anything else: refuse to start, because the engine disables Cloud Run's
//     invoker check and the service would otherwise e-mail on anyone's behalf.
func pushGuard(cfg config, v pushauth.Verifier, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	if cfg.PushServiceAccount != "" {
		pc := pushauth.Config{ServiceAccount: cfg.PushServiceAccount, Audiences: cfg.PushAudiences}
		return func(next http.Handler) http.Handler { return pushauth.Require(v, pc, logger, next) }, nil
	}
	if cfg.DevMode {
		if cfg.OnCloudRun {
			return nil, errors.New("DEV_MODE=true is refused on Cloud Run (K_SERVICE is set): set PUSH_SERVICE_ACCOUNT instead")
		}
		logger.Warn("DEV_MODE: pushes are not authenticated; never use this outside a local run")
		return func(next http.Handler) http.Handler { return next }, nil
	}
	return nil, errors.New("PUSH_SERVICE_ACCOUNT is not set: refusing to accept unauthenticated pushes (set DEV_MODE=true for a local run)")
}

// newRouter registers every route, each wrapped in an OpenTelemetry server
// span.
func newRouter(push http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", telemetry.Handler("GET /healthz", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
		})))
	// Eventarc delivers to the service root by default.
	mux.Handle("POST /{$}", telemetry.Handler("POST /", push))
	return mux
}

func main() {
	if err := run(); err != nil {
		slog.Error("notifier stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg := loadConfig(os.Getenv)
	logger := telemetry.NewLogger(os.Stdout, cfg.ProjectID, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	tcfg := telemetry.Config{ProjectID: cfg.ProjectID, ServiceName: cfg.ServiceName, ServiceVersion: cfg.ServiceVersion}
	shutdownTracing, err := telemetry.SetupTracing(ctx, tcfg)
	if err != nil {
		return err
	}
	// The profiler is best effort: a missing permission must not take the
	// service down.
	if err := telemetry.StartProfiler(tcfg); err != nil {
		logger.Warn("profiler disabled", slog.Any("error", err))
	}

	verifier, err := idtoken.NewValidator(ctx)
	if err != nil {
		return fmt.Errorf("failed to create the push token validator: %w", err)
	}
	guard, err := pushGuard(cfg, verifier, logger)
	if err != nil {
		_ = shutdownTracing(context.Background())
		return err
	}

	m, err := newMailer(cfg, logger)
	if err != nil {
		return err
	}
	push, err := delivery.NewHandler(m, logger, delivery.DefaultDedupeCapacity)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           newRouter(guard(push)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("notifier listening",
			slog.String("port", cfg.Port),
			slog.String("service", cfg.ServiceName),
			slog.String("version", cfg.ServiceVersion),
		)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		_ = shutdownTracing(context.Background())
		return fmt.Errorf("failed to serve HTTP: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelDrain()
	shutdownErr := srv.Shutdown(drainCtx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("failed to drain HTTP server: %w", shutdownErr)
	}

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), flushGrace)
	defer cancelFlush()
	return errors.Join(shutdownErr, shutdownTracing(flushCtx))
}
