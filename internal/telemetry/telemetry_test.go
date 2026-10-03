package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggerUsesCloudLoggingFieldNames(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "proj", slog.LevelDebug)
	logger.Debug("d")
	logger.Info("i", slog.Group("g", slog.String("msg", "inner")))
	logger.Warn("w")
	logger.Error("e")

	lines := decodeLines(t, &buf)
	want := []struct{ severity, message string }{
		{"DEBUG", "d"}, {"INFO", "i"}, {"WARNING", "w"}, {"ERROR", "e"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		l := lines[i]
		if l["severity"] != w.severity || l["message"] != w.message {
			t.Errorf("line %d = %v, want severity %s message %s", i, l, w.severity, w.message)
		}
		if _, ok := l["time"]; !ok {
			t.Errorf("line %d has no time", i)
		}
		for _, k := range []string{"level", "msg"} {
			if _, ok := l[k]; ok {
				t.Errorf("line %d still has slog key %q", i, k)
			}
		}
	}
	// Keys inside groups are user data and must not be renamed.
	if g, _ := lines[1]["g"].(map[string]any); g["msg"] != "inner" {
		t.Errorf("grouped attribute rewritten: %v", lines[1]["g"])
	}
}

func spanContext(t *testing.T, sampled bool) trace.SpanContext {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	flags := trace.TraceFlags(0)
	if sampled {
		flags = trace.FlagsSampled
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: flags})
}

func TestLoggerAddsTraceCorrelation(t *testing.T) {
	var buf bytes.Buffer
	// Derive with With() to prove derived loggers keep correlation.
	logger := NewLogger(&buf, "island-venues-prod", slog.LevelInfo).With(slog.String("k", "v"))
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t, true))

	logger.InfoContext(ctx, "with span")
	logger.Info("without span")

	lines := decodeLines(t, &buf)
	if got := lines[0][TraceKey]; got != "projects/island-venues-prod/traces/4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace = %v", got)
	}
	if got := lines[0][SpanIDKey]; got != "00f067aa0ba902b7" {
		t.Errorf("spanId = %v", got)
	}
	if got := lines[0][TraceSampledKey]; got != true {
		t.Errorf("trace_sampled = %v", got)
	}
	if _, ok := lines[1][TraceKey]; ok {
		t.Errorf("record without span carries a trace: %v", lines[1])
	}
}

func TestLoggerWithoutProjectOmitsTraceName(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "", slog.LevelInfo)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t, false))
	logger.InfoContext(ctx, "x")
	l := decodeLines(t, &buf)[0]
	if _, ok := l[TraceKey]; ok {
		t.Errorf("trace emitted without a project: %v", l)
	}
	if l[SpanIDKey] != "00f067aa0ba902b7" || l[TraceSampledKey] != false {
		t.Errorf("span fields missing: %v", l)
	}
}

// Cloud Logging ignores correlation keys nested in a group, so they must stay
// top level whatever groups the logger was derived with, while the record's
// own attributes keep their grouping.
func TestLoggerKeepsCorrelationTopLevelUnderGroups(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "p", slog.LevelInfo).
		With(slog.String("a", "1")).
		WithGroup("req").
		With(slog.String("b", "2")).
		WithGroup("inner")
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t, true))
	logger.InfoContext(ctx, "x", slog.String("c", "3"))

	l := decodeLines(t, &buf)[0]
	if l[TraceKey] != "projects/p/traces/4bf92f3577b34da6a3ce929d0e0e4736" || l[SpanIDKey] != "00f067aa0ba902b7" {
		t.Errorf("correlation not top level: %v", l)
	}
	req, _ := l["req"].(map[string]any)
	inner, _ := req["inner"].(map[string]any)
	if l["a"] != "1" || req["b"] != "2" || inner["c"] != "3" {
		t.Errorf("grouping changed: %v", l)
	}
	if _, ok := inner[SpanIDKey]; ok {
		t.Errorf("correlation duplicated inside group: %v", l)
	}
}

// TestHandlerJoinsIncomingTrace checks the end-to-end promise of the talk:
// a request arriving with a Google or W3C trace header produces log lines that
// Cloud Logging attaches to that same trace, even with tracing export off.
func TestHandlerJoinsIncomingTrace(t *testing.T) {
	cases := map[string]struct{ header, value string }{
		"X-Cloud-Trace-Context": {"X-Cloud-Trace-Context", "4bf92f3577b34da6a3ce929d0e0e4736/123;o=1"},
		"traceparent":           {"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, "p", slog.LevelInfo)
			h := Handler("GET /x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logger.InfoContext(r.Context(), "handled")
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(c.header, c.value)
			h.ServeHTTP(httptest.NewRecorder(), req)

			l := decodeLines(t, &buf)[0]
			if got := l[TraceKey]; got != "projects/p/traces/4bf92f3577b34da6a3ce929d0e0e4736" {
				t.Errorf("trace = %v, want the incoming trace id", got)
			}
		})
	}
}

func TestSetupTracingWithoutProjectIsNoop(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), Config{ServiceName: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := StartProfiler(Config{ServiceName: "s"}); err != nil {
		t.Fatalf("profiler without project: %v", err)
	}
}

func TestThirdPartyTransportDoesNotLeakTraceHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t, true))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := (&http.Client{Transport: ThirdPartyTransport(http.DefaultTransport)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, h := range []string{"Traceparent", "X-Cloud-Trace-Context"} {
		if v := got.Get(h); v != "" {
			t.Errorf("third party received %s: %s", h, v)
		}
	}
}

func TestNewResourceNamesService(t *testing.T) {
	res, err := newResource(Config{ServiceName: "island-venues-notifier", ServiceVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	attrs := map[string]string{}
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["service.name"] != "island-venues-notifier" || attrs["service.version"] != "1.0.0" {
		t.Errorf("resource attributes = %v", attrs)
	}
}
