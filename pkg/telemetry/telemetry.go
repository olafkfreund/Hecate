// Package telemetry configures OpenTelemetry tracing from the standard OTEL_*
// environment, so Hecate drops into an existing collector setup with no
// Hecate-specific configuration at all.
//
// It is off unless asked for. The OpenTelemetry SDK's own default is to export
// to localhost:4318, which for the overwhelming majority of installations means
// a controller logging connection failures forever about a collector nobody
// deployed. Silence is the better default; one environment variable turns it on.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Enabled reports whether the environment asks for tracing.
//
// `OTEL_TRACES_EXPORTER=none` is the specified way to turn it off and wins over
// everything else; otherwise an endpoint — general or traces-specific — is what
// says a collector exists.
func Enabled() bool {
	switch os.Getenv("OTEL_TRACES_EXPORTER") {
	case "none":
		return false
	case "otlp":
		return true
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Start installs the global tracer provider and propagator.
//
// The returned shutdown flushes buffered spans; it is never nil, so callers can
// defer it unconditionally. When tracing is disabled Start does nothing and
// reports it, leaving OpenTelemetry's no-op provider in place — every
// tracer.Start in the codebase then costs an interface call and produces an
// invalid span context, which is exactly what the callers already handle.
func Start(ctx context.Context, service, version string) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, false, nil
	}

	exporter, err := spanExporter(ctx)
	if err != nil {
		return noop, false, fmt.Errorf("configuring the OTLP trace exporter: %w", err)
	}

	// resource.New merges our defaults *under* the environment, so
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override what we pass —
	// the operator running three Hecates has to be able to tell them apart.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(service),
			semconv.ServiceVersion(version),
		),
		resource.WithHost(),
		resource.WithFromEnv(),
	)
	if err != nil {
		// Partial resources are returned alongside an error — a schema-URL
		// conflict, typically. Losing an attribute is not a reason to run
		// untraced, so carry on with what we got.
		if res == nil {
			return noop, false, fmt.Errorf("building the telemetry resource: %w", err)
		}
	}

	// The sampler is deliberately not configured here: the SDK reads
	// OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG itself, and passing one
	// would silently override the operator's.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return tp.Shutdown, true, nil
}

// spanExporter picks the OTLP transport the standard environment asks for.
//
// Each exporter reads endpoint, headers, TLS and timeout from the environment
// itself, so only the protocol has to be decided here.
func spanExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch protocol {
	case "grpc":
		return otlptracegrpc.New(ctx)
	case "", "http/protobuf":
		// The specified default, and what most collectors expose.
		return otlptracehttp.New(ctx)
	default:
		return nil, fmt.Errorf(
			"unsupported OTLP protocol %q: hecate speaks grpc and http/protobuf", protocol)
	}
}

// Attr is a convenience for the hecate.* attributes shared by traces.
func Attr(key, value string) attribute.KeyValue {
	return attribute.String("hecate."+key, value)
}

// NewTraceID returns a fresh W3C trace ID as 32 hex characters.
//
// Allocated when a Passage starts rather than when its trace is emitted,
// because a `traceparent` git trailer has to be written *during* the crossing —
// long before the trace exists. Persisting it in `status.traceID` also keeps
// commits reproducible: a retried Passage writes the same trailer, so it
// produces the same commit SHA (D42).
func NewTraceID() string {
	var id [16]byte
	for {
		if _, err := rand.Read(id[:]); err != nil {
			// crypto/rand does not fail on any platform we run on, and a
			// promotion must not be blocked by telemetry. No ID means no trace.
			return ""
		}
		// Both halves must be non-zero: the second is the parent span ID
		// (Traceparent below), and an all-zero span ID is invalid.
		if !bytes.Equal(id[:8], zero8) && !bytes.Equal(id[8:], zero8) {
			return hex.EncodeToString(id[:])
		}
	}
}

var zero8 = make([]byte, 8)

// Traceparent renders a W3C `traceparent` header value for a Passage's trace.
//
// **The parent span ID is the trace ID's own second half.** It therefore needs
// no field of its own — anyone holding `status.traceID` can recompute the exact
// trailer that was written — and it names a span Hecate never emits: the
// promotion commit itself. Git is the rendezvous (D3), so the commit is where
// the crossing and whatever reconciles it downstream both hang from.
//
// Returns "" for anything that is not a valid trace ID, so a caller can treat
// "no trace" and "no trailer" as the same case.
func Traceparent(traceID string) string {
	if len(traceID) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		return ""
	}
	if traceID[:16] == "0000000000000000" || traceID[16:] == "0000000000000000" {
		return ""
	}
	// version 00, sampled: Hecate only allocates a trace ID when it is
	// exporting, so a trailer that exists describes a trace that exists.
	return "00-" + traceID + "-" + traceID[16:] + "-01"
}
