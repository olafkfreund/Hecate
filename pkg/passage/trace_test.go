package passage

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// recorder installs a real SDK tracer provider that keeps spans in memory, and
// restores the previous global on cleanup so tests stay independent.
func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return sr
}

// The trace ID a Passage is given when it starts, so git-commit can write a
// traceparent trailer long before the trace exists.
const allocatedTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func at(base time.Time, seconds int) *metav1.Time {
	return &metav1.Time{Time: base.Add(time.Duration(seconds) * time.Second)}
}

func finishedPassage(base time.Time) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-1", Namespace: "apps"},
		Spec: v1alpha1.PassageSpec{
			Gate: "production", Bundle: "app-abc", Actor: "olaf",
		},
		Status: v1alpha1.PassageStatus{
			Phase:      v1alpha1.PassageSucceeded,
			TraceID:    allocatedTraceID,
			StartedAt:  at(base, 0),
			FinishedAt: at(base, 90),
			Steps: []v1alpha1.StepStatus{
				{
					Uses: "git-clone", Phase: v1alpha1.StepSucceeded,
					StartedAt: at(base, 0), FinishedAt: at(base, 2),
				},
				{
					Uses: "flux-wait", As: "converge", Phase: v1alpha1.StepSucceeded,
					StartedAt: at(base, 2), FinishedAt: at(base, 90),
				},
			},
		},
	}
}

func TestRecordTraceEmitsAPassageAndItsSteps(t *testing.T) {
	sr := recorder(t)
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	recordTrace(finishedPassage(base))

	spans := sr.Ended()
	if len(spans) != 3 {
		t.Fatalf("want a root span and two step spans, got %d", len(spans))
	}

	// Steps end before the root, so the recorder sees them first.
	clone, wait, root := spans[0], spans[1], spans[2]
	if root.Name() != "passage production" {
		t.Errorf("root span name = %q", root.Name())
	}
	// The trace must be the one the commit trailer already promised, or the
	// trailer points at a trace that does not exist.
	if got := root.SpanContext().TraceID().String(); got != allocatedTraceID {
		t.Errorf("trace ID = %q, want the one allocated at the start (%q)",
			got, allocatedTraceID)
	}
	// Parented under the span the trailer names — the promotion commit, which
	// Hecate never emits itself.
	if got := root.Parent().SpanID().String(); got != allocatedTraceID[16:] {
		t.Errorf("root parent = %q, want the trailer's parent span %q",
			got, allocatedTraceID[16:])
	}

	// The point of the whole exercise: one trace, steps under the Passage.
	for _, s := range []sdktrace.ReadOnlySpan{clone, wait} {
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%s is in a different trace from its Passage", s.Name())
		}
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s is not a child of the Passage span", s.Name())
		}
	}

	if got := wait.Name(); got != "flux-wait (converge)" {
		t.Errorf("aliased step span name = %q", got)
	}
	// The number nobody currently measures: how long Flux actually took.
	if got := wait.EndTime().Sub(wait.StartTime()); got != 88*time.Second {
		t.Errorf("flux-wait span duration = %s, want 88s — spans must replay the "+
			"recorded timestamps, not the time recordTrace ran", got)
	}
	if got := root.EndTime().Sub(root.StartTime()); got != 90*time.Second {
		t.Errorf("passage span duration = %s, want 90s", got)
	}
}

func TestRecordTraceMarksFailures(t *testing.T) {
	sr := recorder(t)
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	p := finishedPassage(base)
	p.Status.Phase = v1alpha1.PassageFailed
	p.Status.Message = "crossing production failed: no such repository"
	p.Status.Steps[1].Phase = v1alpha1.StepFailed
	p.Status.Steps[1].Message = "no such repository"
	p.Status.Steps[1].Reason = "NotFound"

	recordTrace(p)

	spans := sr.Ended()
	failed, root := spans[1], spans[2]
	if failed.Status().Code != codes.Error {
		t.Errorf("failed step span status = %v, want Error", failed.Status().Code)
	}
	if root.Status().Code != codes.Error {
		t.Errorf("failed Passage span status = %v, want Error", root.Status().Code)
	}
	if !hasAttr(failed, "hecate.step.reason", "NotFound") {
		t.Errorf("failed step span is missing the reason attribute: %v", failed.Attributes())
	}
}

// A step after the one that failed never ran. Recording it would put a
// zero-length span at the epoch in the middle of the trace.
func TestRecordTraceSkipsStepsThatNeverRan(t *testing.T) {
	sr := recorder(t)
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	p := finishedPassage(base)
	p.Status.Steps = append(p.Status.Steps,
		v1alpha1.StepStatus{Uses: "git-push", Phase: v1alpha1.StepPending})

	recordTrace(p)

	if got := len(sr.Ended()); got != 3 {
		t.Fatalf("want 3 spans with the unrun step left out, got %d", got)
	}
}

// Without a configured exporter OpenTelemetry's no-op provider is in place, and
// a traceID pointing at a trace nobody exported is worse than an empty field.
func TestRecordTraceReturnsNothingWhenTracingIsOff(t *testing.T) {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	recordTrace(finishedPassage(base))
}

// No trace ID means tracing was off when the Passage started: no trailer was
// written, so there is nothing to emit and nothing to emit it under.
func TestRecordTraceEmitsNothingWithoutAnAllocatedID(t *testing.T) {
	sr := recorder(t)
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	p := finishedPassage(base)
	p.Status.TraceID = ""
	recordTrace(p)

	if got := len(sr.Ended()); got != 0 {
		t.Errorf("want no spans without an allocated trace ID, got %d", got)
	}
}

func TestRecordTraceIgnoresAPassageThatNeverStarted(t *testing.T) {
	sr := recorder(t)

	p := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-1", Namespace: "apps"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "gone"},
		Status: v1alpha1.PassageStatus{
			Phase:      v1alpha1.PassageFailed,
			TraceID:    allocatedTraceID,
			Message:    "Bundle gone no longer exists",
			FinishedAt: at(time.Now(), 0),
		},
	}
	recordTrace(p)
	if got := len(sr.Ended()); got != 0 {
		t.Errorf("want no spans, got %d", got)
	}
}

func hasAttr(s sdktrace.ReadOnlySpan, key, value string) bool {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key && kv.Value.AsString() == value {
			return true
		}
	}
	return false
}
