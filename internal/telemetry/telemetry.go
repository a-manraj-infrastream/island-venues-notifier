package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"cloud.google.com/go/profiler"
	// Both Google packages below are marked deprecated upstream in favour of
	// OTLP export and W3C-only propagation. They are used deliberately: the
	// service contract names the Cloud Trace exporter, and Cloud Run and
	// Pub/Sub push still send X-Cloud-Trace-Context. Migrating to OTLP
	// (telemetry.googleapis.com) should happen before the exporter is archived
	// in January 2027.
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace" //nolint:staticcheck // see comment above
	gcppropagator "github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator" //nolint:staticcheck // see comment above
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Config holds the identity of the running service. It is filled from the
// environment by main; this package never reads environment variables itself.
type Config struct {
	// ProjectID enables Cloud Trace export and the profiler when non-empty.
	ProjectID      string
	ServiceName    string
	ServiceVersion string
}

// ShutdownFunc flushes buffered telemetry. It must be called on exit so the
// spans of the last requests before a Cloud Run SIGTERM are not lost.
type ShutdownFunc func(context.Context) error

// Propagator understands both the W3C traceparent header and Google's
// X-Cloud-Trace-Context header. Cloud Run's front end and Pub/Sub push
// deliveries still stamp X-Cloud-Trace-Context, so extracting it is what lets a
// request span join the trace the platform already started.
//
// Extraction runs left to right and a later propagator overwrites an earlier
// one, so traceparent wins when a caller sends both.
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		gcppropagator.CloudTraceFormatPropagator{}, //nolint:staticcheck // deliberate, see imports
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// SetupTracing installs the global propagator and, when a project is
// configured, a tracer provider exporting to Cloud Trace. Without a project the
// global tracer stays the OpenTelemetry no-op: incoming trace context is still
// propagated (so logs correlate) but nothing is recorded or exported.
func SetupTracing(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	otel.SetTextMapPropagator(Propagator())

	if cfg.ProjectID == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := texporter.New(texporter.WithProjectID(cfg.ProjectID)) //nolint:staticcheck // deliberate, see imports
	if err != nil {
		return nil, fmt.Errorf("failed to create Cloud Trace exporter: %w", err)
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// Honour the caller's sampling decision (Cloud Run samples its own
		// front-end traces); sample everything we originate. The service
		// handles a handful of events per booking, so volume is not a concern.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to flush tracer provider: %w", err)
		}
		return nil
	}, nil
}

// newResource describes the service to Cloud Trace. The service attributes are
// schemaless on purpose: merging two resources that carry different semconv
// schema URLs fails, and the SDK default resource moves to a newer schema on
// most OpenTelemetry releases.
func newResource(cfg Config) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("failed to build trace resource: %w", err)
	}
	return res, nil
}

// StartProfiler starts the Cloud Profiler agent. It is a no-op without a
// project so local runs and tests never try to reach Google APIs.
func StartProfiler(cfg Config) error {
	if cfg.ProjectID == "" {
		return nil
	}
	if cfg.ServiceName == "" {
		return errors.New("failed to start profiler: service name is required")
	}
	if err := profiler.Start(profiler.Config{
		Service:        cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		ProjectID:      cfg.ProjectID,
	}); err != nil {
		return fmt.Errorf("failed to start profiler: %w", err)
	}
	return nil
}

// Handler wraps h in an OpenTelemetry server span named after the route. The
// propagator is passed explicitly rather than read from the global so the
// behaviour does not depend on SetupTracing having run (it matters in tests).
func Handler(route string, h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, route, otelhttp.WithPropagators(Propagator()))
}

// ThirdPartyTransport wraps an outbound RoundTripper so calls to an external
// API appear as client spans under the request span. It deliberately injects
// no trace headers: the only dependency is a third-party SaaS, which has no use
// for our trace ids and should not receive them.
func ThirdPartyTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base, otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()))
}
