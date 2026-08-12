package passage

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/telemetry"
)

const tracerName = "github.com/olafkfreund/hecate/pkg/passage"

// recordTrace emits a finished Passage as one trace — the Passage a root span,
// each step a child — and returns the trace ID for `status.traceID`.
//
// **The trace is reconstructed from the persisted status rather than held open
// across the crossing.** A Passage spans many reconciles and can outlive the
// controller process: a live span tree would be lost by any restart, and a
// crossing that waits an hour for Flux is exactly the one worth tracing. Status
// is the only thing that survives, so status is what we trace from, replaying
// the recorded timestamps with trace.WithTimestamp.
//
// The cost is that the trace appears when the Passage finishes, not while it
// runs. That is the trade a durable record buys. #33 needs the trace ID *during*
// the crossing to write a `traceparent` commit trailer, and will have to
// allocate the ID up front and seed a parent span context here — a contained
// change, and one with a reason to exist by then.
//
// Returns "" when tracing is off: the no-op provider yields an invalid span
// context, and a traceID field pointing at a trace nobody exported would be
// worse than an empty one.
func recordTrace(p *v1alpha1.Passage) string {
	if p.Status.StartedAt == nil || p.Status.FinishedAt == nil {
		// A Passage that failed before its first step ran — no span has an
		// honest duration, so there is nothing to say.
		return ""
	}

	tracer := otel.Tracer(tracerName)
	ctx, root := tracer.Start(context.Background(), "passage "+p.Spec.Gate,
		trace.WithTimestamp(p.Status.StartedAt.Time),
		trace.WithAttributes(
			telemetry.Attr("gate", p.Spec.Gate),
			telemetry.Attr("bundle", p.Spec.Bundle),
			telemetry.Attr("passage", p.Name),
			telemetry.Attr("namespace", p.Namespace),
			telemetry.Attr("actor", p.Spec.Actor),
			telemetry.Attr("phase", string(p.Status.Phase)),
		))

	for _, st := range p.Status.Steps {
		traceStep(ctx, tracer, st)
	}

	if p.Status.Phase != v1alpha1.PassageSucceeded {
		root.SetStatus(codes.Error, p.Status.Message)
	}
	root.End(trace.WithTimestamp(p.Status.FinishedAt.Time))

	if sc := root.SpanContext(); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// traceStep emits one step's span.
//
// A step with no StartedAt never ran — everything after the failure that ended
// the Passage. Recording those as zero-length spans at the epoch would be
// actively misleading, so they are left out; the Passage status still lists them.
func traceStep(ctx context.Context, tracer trace.Tracer, st v1alpha1.StepStatus) {
	if st.StartedAt == nil {
		return
	}
	name := st.Uses
	if st.As != "" {
		name += " (" + st.As + ")"
	}

	_, span := tracer.Start(ctx, name,
		trace.WithTimestamp(st.StartedAt.Time),
		trace.WithAttributes(
			telemetry.Attr("step.uses", st.Uses),
			telemetry.Attr("step.phase", string(st.Phase)),
		))
	if st.Reason != "" {
		span.SetAttributes(telemetry.Attr("step.reason", st.Reason))
	}
	if st.Phase == v1alpha1.StepFailed {
		span.SetStatus(codes.Error, st.Message)
	}

	// An unfinished step at this point was aborted mid-flight; ending it at the
	// Passage's finish is the closest honest answer.
	end := st.FinishedAt
	if end == nil {
		span.End()
		return
	}
	span.End(trace.WithTimestamp(end.Time))
}
