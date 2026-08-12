package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// The default matters more than anything else here: the OpenTelemetry SDK would
// otherwise export to localhost:4318, and every installation without a
// collector would log connection failures forever.
func TestEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"an endpoint means a collector exists",
			map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, true},
		{"a traces-specific endpoint counts too",
			map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318"}, true},
		{"explicit otlp with no endpoint takes the SDK default",
			map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, true},
		{"none wins over an endpoint", map[string]string{
			"OTEL_TRACES_EXPORTER":        "none",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartIsANoOpWhenDisabled(t *testing.T) {
	shutdown, enabled, err := Start(context.Background(), "hecate-controller", "test")
	if err != nil {
		t.Fatalf("Start with no configuration: %v", err)
	}
	if enabled {
		t.Error("tracing reported enabled with no OTEL_* configuration")
	}
	// Never nil, so callers can defer it unconditionally.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestStartRefusesAnUnsupportedProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")

	shutdown, enabled, err := Start(context.Background(), "hecate-controller", "test")
	if err == nil {
		t.Fatal("want an error for an unsupported OTLP protocol, got none")
	}
	if enabled {
		t.Error("tracing reported enabled after a failed start")
	}
	if shutdown == nil {
		t.Error("shutdown must be safe to defer even when Start failed")
	}
}

// The exporter is lazy — it does not dial until the first export — so this
// starts cleanly against an endpoint nothing is listening on, which is exactly
// what makes it testable.
func TestStartEnablesTracing(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	shutdown, enabled, err := Start(context.Background(), "hecate-controller", "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !enabled {
		t.Error("tracing reported off despite a configured endpoint")
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}

// A recording SDK proves spans are built; it does not prove any of them leave
// the process. This drives the configured exporter against a real HTTP endpoint
// and checks a payload arrives, which is what catches a wrong protocol default,
// a wrong path, or a shutdown that does not flush.
func TestStartExportsSpansOverOTLPHTTP(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case got <- r.URL.Path + " " + strconv.Itoa(len(body)):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	// httptest speaks plaintext; without this the exporter insists on TLS.
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	shutdown, enabled, err := Start(context.Background(), "hecate-controller", "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !enabled {
		t.Fatal("tracing reported off despite a configured endpoint")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "a crossing")
	span.End()

	// Shutdown flushes the batch processor, so the export must have happened by
	// the time it returns.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case req := <-got:
		if !strings.HasPrefix(req, "/v1/traces ") {
			t.Errorf("spans were posted to %q, want /v1/traces", req)
		}
		if strings.HasSuffix(req, " 0") {
			t.Error("the collector received an empty body")
		}
	default:
		t.Fatal("no span reached the collector")
	}
}

func TestNewTraceIDIsUsableAsATraceparent(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewTraceID()
		if len(id) != 32 {
			t.Fatalf("trace ID %q is not 32 hex characters", id)
		}
		if seen[id] {
			t.Fatalf("NewTraceID repeated %q", id)
		}
		seen[id] = true

		// The second half doubles as the parent span ID, so it must never be
		// all-zero — that is the invalid span ID, and a backend drops it.
		if id[16:] == "0000000000000000" || id[:16] == "0000000000000000" {
			t.Fatalf("trace ID %q has a zero half", id)
		}
		if tp := Traceparent(id); tp != "00-"+id+"-"+id[16:]+"-01" {
			t.Fatalf("Traceparent(%q) = %q", id, tp)
		}
	}
}

func TestTraceparentRefusesWhatIsNotATraceID(t *testing.T) {
	for _, bad := range []string{
		"",
		"too short",
		"00000000000000000000000000000000",   // all zero
		"0000000000000000ffffffffffffffff",   // zero trace half
		"ffffffffffffffff0000000000000000",   // zero span half
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",   // not hex
		"ffffffffffffffffffffffffffffffffff", // too long
	} {
		if got := Traceparent(bad); got != "" {
			t.Errorf("Traceparent(%q) = %q, want empty", bad, got)
		}
	}
}
