// Package telemetry wires the three observability signals of the service:
// structured logs that Cloud Logging understands, OpenTelemetry traces exported
// to Cloud Trace, and the continuous profiler.
//
// Everything here degrades to a local-only mode when no Google Cloud project is
// configured, so tests and laptops never need credentials.
package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Cloud Logging reads these well-known keys from a JSON log line and uses them
// to attach the entry to a trace in Cloud Trace. See
// https://cloud.google.com/logging/docs/structured-logging#special-payload-fields.
const (
	TraceKey        = "logging.googleapis.com/trace"
	SpanIDKey       = "logging.googleapis.com/spanId"
	TraceSampledKey = "logging.googleapis.com/trace_sampled"
)

// NewLogger returns a JSON slog logger whose field names match what Cloud
// Logging expects (severity, message, time) and which stamps every record
// written with a context carrying a span with the trace correlation fields.
//
// projectID is needed to build the fully qualified trace name
// (projects/<project>/traces/<traceId>); when it is empty the trace field is
// omitted because a bare trace id is not accepted by Cloud Logging, but the
// span id is still written so local logs remain correlatable.
func NewLogger(w io.Writer, projectID string, level slog.Leveler) *slog.Logger {
	json := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceCloudLoggingKeys,
	})
	return slog.New(&traceHandler{root: json, derived: json, projectID: projectID})
}

// replaceCloudLoggingKeys renames slog's default keys to Cloud Logging's.
// Only top-level keys are rewritten: a user attribute called "msg" inside a
// group must not be mistaken for the record message.
func replaceCloudLoggingKeys(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.MessageKey:
		a.Key = "message"
	case slog.LevelKey:
		a.Key = "severity"
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(severity(lvl))
		}
	}
	// slog.TimeKey is already "time", which Cloud Logging accepts as an
	// RFC 3339 timestamp, so it is left untouched.
	return a
}

// severity maps slog levels onto Cloud Logging's LogSeverity names. slog uses
// "WARN" whereas Cloud Logging only recognises "WARNING"; an unrecognised value
// silently becomes DEFAULT, which would hide warnings from severity filters.
func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// traceHandler decorates records with the span context found in the context
// passed to the *Context logging methods (InfoContext, ErrorContext, ...).
//
// Cloud Logging only reads the correlation keys at the top level of the JSON
// payload. Attributes added to a record land inside the innermost open group,
// so once a group is open the handler rebuilds the chain from the root with
// the correlation keys first. Without groups (the common case) it simply adds
// them to the record.
type traceHandler struct {
	root      slog.Handler                      // handler with nothing derived
	derived   slog.Handler                      // root with every With/WithGroup applied
	steps     []func(slog.Handler) slog.Handler // how derived was built from root
	grouped   bool                              // a WithGroup is among the steps
	projectID string
}

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.derived.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.derived.Handle(ctx, r)
	}
	attrs := make([]slog.Attr, 0, 3)
	if h.projectID != "" {
		attrs = append(attrs, slog.String(TraceKey, "projects/"+h.projectID+"/traces/"+sc.TraceID().String()))
	}
	attrs = append(attrs,
		slog.String(SpanIDKey, sc.SpanID().String()),
		slog.Bool(TraceSampledKey, sc.IsSampled()),
	)
	if !h.grouped {
		r.AddAttrs(attrs...)
		return h.derived.Handle(ctx, r)
	}
	out := h.root.WithAttrs(attrs)
	for _, step := range h.steps {
		out = step(out)
	}
	return out.Handle(ctx, r)
}

// WithAttrs and WithGroup must return a traceHandler again, otherwise a logger
// derived with logger.With(...) would silently lose trace correlation.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.with(func(x slog.Handler) slog.Handler { return x.WithAttrs(attrs) }, false)
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(func(x slog.Handler) slog.Handler { return x.WithGroup(name) }, true)
}

func (h *traceHandler) with(step func(slog.Handler) slog.Handler, group bool) *traceHandler {
	steps := make([]func(slog.Handler) slog.Handler, len(h.steps), len(h.steps)+1)
	copy(steps, h.steps)
	return &traceHandler{
		root:      h.root,
		derived:   step(h.derived),
		steps:     append(steps, step),
		grouped:   h.grouped || group,
		projectID: h.projectID,
	}
}
