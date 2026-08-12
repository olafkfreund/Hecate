package passage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/expr"
	"github.com/olafkfreund/hecate/pkg/telemetry"
)

// Engine advances a Passage by running its steps in order.
//
// It is deliberately not a long-running loop. Each call to Advance runs as far
// as it can and returns; the controller persists the resulting status and calls
// again. All progress therefore lives in the Passage object rather than in
// process memory, so a restart resumes mid-Passage instead of starting over.
type Engine struct {
	Registry *Registry
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

const (
	// ReasonUnregisteredStep is set when a Passage names a step that does not exist.
	ReasonUnregisteredStep = "UnregisteredStep"
	// ReasonBadExpression is set when a step's `with:` or `if:` cannot be
	// evaluated. A malformed expression will not become valid by retrying.
	ReasonBadExpression = "BadExpression"
)

// Outcome is what Advance concluded.
type Outcome struct {
	// Status is the Passage's updated status. The caller persists it.
	Status v1alpha1.PassageStatus
	// RequeueAfter is how long to wait before calling Advance again. Zero means
	// the Passage is finished.
	RequeueAfter time.Duration
	// Watch accumulates the health checks emitted by steps, for the Gate to
	// adopt once the Passage succeeds.
	Watch []v1alpha1.HealthCheck
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Advance runs the Passage from wherever it left off.
//
// workDir is the scratch directory shared by this Passage's steps; the caller
// owns its lifecycle. Passed in rather than resolved through a callback,
// because the caller already knows it and a callback would only add a way to
// forget to set it.
func (e *Engine) Advance(
	ctx context.Context, p *v1alpha1.Passage, bundle *v1alpha1.Bundle, workDir string,
) Outcome {
	status := *p.Status.DeepCopy()
	now := e.now()

	if status.Phase == "" {
		status.Phase = v1alpha1.PassagePending
	}
	if status.Phase.Terminal() {
		return Outcome{Status: status}
	}

	// Abort is checked before any step runs, so a cancellation requested while
	// waiting takes effect at the next tick rather than after the current wait.
	if p.Spec.Abort {
		return Outcome{Status: e.abort(status)}
	}

	// Grow the status to match the spec exactly once, so indices line up for
	// the life of the Passage.
	if len(status.Steps) != len(p.Spec.Steps) {
		status.Steps = make([]v1alpha1.StepStatus, len(p.Spec.Steps))
		for i, s := range p.Spec.Steps {
			status.Steps[i] = v1alpha1.StepStatus{Uses: s.Uses, As: s.As, Phase: v1alpha1.StepPending}
		}
	}
	if status.StartedAt == nil {
		status.StartedAt = &metav1.Time{Time: now}
	}
	status.Phase = v1alpha1.PassageRunning

	outputs := collectOutputs(status.Steps)
	var watch []v1alpha1.HealthCheck

	// Once a step has failed fatally the Passage is going to fail — but the
	// remaining steps are not simply abandoned. A step that asked to run anyway
	// still runs, which is what lets a crossing report its own outcome, record
	// evidence of a failure, or tidy up after itself. Everything else is
	// Skipped (D46).
	//
	// This is a local rather than persisted state because it cannot outlive the
	// call: a Passage that has failed is terminal, and the controller never
	// calls Advance on a terminal Passage again. There is nothing to resume.
	failed, failure := false, ""

	vars := make(map[string]string, len(p.Spec.Vars))
	for _, v := range p.Spec.Vars {
		vars[v.Name] = v.Value
	}

	for i := int(status.CurrentStep); i < len(p.Spec.Steps); i++ {
		step := p.Spec.Steps[i]
		st := &status.Steps[i]
		if !failed {
			// Frozen at the first failure. The loop carries on past it to run
			// whatever asked to run anyway, but `currentStep` is what a reader
			// consults for "where did this get to", and marching it to the end
			// would point at a step that was skipped rather than at the one
			// that broke. A failed Passage never resumes, so nothing needs it
			// as a cursor.
			status.CurrentStep = int32(i)
		}

		if st.Phase.Terminal() {
			continue
		}

		// Rebuilt per step, because `steps.<alias>` grows as earlier steps
		// finish — an env computed once at the top would hide every output.
		env := expr.Env{
			Bundle: bundle, Steps: outputs, Vars: vars, Failed: failed,
			Gate: p.Spec.Gate, Passage: p.Name, Actor: p.Spec.Actor, Namespace: p.Namespace,
		}

		// A step gated off is recorded as Skipped, never silently omitted: a
		// Passage whose status does not mention a step is indistinguishable
		// from one where the step was deleted.
		if step.If != "" {
			run, err := expr.Bool(step.If, env)
			switch {
			case err != nil:
				st.Reason = ReasonBadExpression
				e.finishStep(st, v1alpha1.StepFailed, fmt.Sprintf("condition: %s", err))
				failed, failure = true, firstOf(failure, st.Message)
				continue
			case !run:
				e.finishStep(st, v1alpha1.StepSkipped, fmt.Sprintf("condition %q was false", step.If))
				continue
			}
		} else if failed {
			// No condition at all means the happy path, and the happy path is
			// over. Saying so beats leaving the step Pending, which reads as
			// "about to run".
			e.finishStep(st, v1alpha1.StepSkipped, "the Passage had already failed")
			continue
		}

		runner, ok := e.Registry.Get(step.Uses)
		if !ok {
			// An unknown step is a configuration error, not a transient fault.
			st.Reason = ReasonUnregisteredStep
			e.finishStep(st, v1alpha1.StepFailed, fmt.Sprintf(
				"no step named %q is registered (available: %v)", step.Uses, e.Registry.Names()))
			failed, failure = true, firstOf(failure, st.Message)
			continue
		}

		if st.StartedAt == nil {
			st.StartedAt = &metav1.Time{Time: e.now()}
		}
		st.Phase = v1alpha1.StepRunning
		st.Attempts++

		var cfg json.RawMessage
		if step.With != nil {
			// Interpolated here rather than in each step, so every step gets it
			// and a step added later cannot forget.
			interpolated, err := expr.InterpolateJSON(step.With.Raw, env)
			if err != nil {
				st.Reason = ReasonBadExpression
				e.finishStep(st, v1alpha1.StepFailed, err.Error())
				failed, failure = true, firstOf(failure, st.Message)
				continue
			}
			cfg = interpolated
		}

		res, err := runner.Run(ctx, &StepContext{
			Namespace:   p.Namespace,
			Traceparent: telemetry.Traceparent(status.TraceID),
			Gate:        p.Spec.Gate,
			Passage:     p.Name,
			Bundle:      bundle,
			Actor:       p.Spec.Actor,
			WorkDir:     workDir,
			Config:      cfg,
			Outputs:     outputs,
			Attempt:     st.Attempts - 1,
			StartedAt:   status.StartedAt.Time,
		})

		// A step that produced output records it even on failure — the output
		// is often the only evidence of what went wrong.
		if len(res.Output) > 0 {
			if encoded, mErr := json.Marshal(res.Output); mErr == nil {
				st.Output = &apiextensionsv1.JSON{Raw: encoded}
			}
			if step.As != "" {
				outputs[step.As] = res.Output
			}
		}
		watch = append(watch, res.Watch...)
		// Last writer wins: a step that re-read the change gate has a fresher
		// verdict than the one before it, and an evidence record that lagged
		// behind the crossing would be worse than none.
		if res.Evidence != nil {
			status.Evidence = res.Evidence
		}

		switch {
		case err != nil:
			e.failStep(st, err)
			// A terminal error is fatal whatever the step asked for: it is the
			// engine saying this will not come right by trying again.
			if IsTerminal(err) || !step.ContinueOnError {
				failed, failure = true, firstOf(failure, err.Error())
			}

		case res.Phase == v1alpha1.StepRunning:
			if failed {
				// Waiting is not available after a failure: the Passage is
				// terminal at the end of this call, so there is no later
				// reconcile to come back in. A step that runs after a failure
				// must finish in one invocation (D46).
				e.finishStep(st, v1alpha1.StepFailed,
					"a step that runs after a failure cannot wait: "+res.Message)
				break
			}
			// Still waiting. Persist progress and come back.
			st.Message = res.Message
			retry := res.RetryAfter
			if retry <= 0 {
				retry = 15 * time.Second
			}
			status.Phase = v1alpha1.PassageRunning
			status.Message = res.Message
			return Outcome{Status: status, RequeueAfter: retry, Watch: watch}

		default:
			phase := res.Phase
			if phase == "" {
				phase = v1alpha1.StepSucceeded
			}
			e.finishStep(st, phase, res.Message)
			if phase == v1alpha1.StepFailed && !step.ContinueOnError {
				failed, failure = true, firstOf(failure, res.Message)
			}
		}
	}

	if failed {
		return Outcome{Status: e.fail(status, failure), Watch: watch}
	}

	status.Phase = v1alpha1.PassageSucceeded
	status.Message = fmt.Sprintf("crossed %s", p.Spec.Gate)
	status.FinishedAt = &metav1.Time{Time: e.now()}
	return Outcome{Status: status, Watch: watch}
}

// finishStep stamps a step's outcome with the clock as it is *now*, not as it
// was when Advance started.
//
// One reading for a whole Advance would give every step that ran inside a
// single reconcile the same start and end — a zero duration for work that
// plainly took time. Both the step spans and hecate_step_duration_seconds are
// then measuring nothing.
// firstOf keeps the earliest failure message.
//
// The first fatal failure is the cause; anything a later always-step reports is
// a consequence of it, and overwriting would leave the Passage explaining the
// symptom rather than the problem.
func firstOf(existing, next string) string {
	if existing != "" {
		return existing
	}
	return next
}

func (e *Engine) finishStep(st *v1alpha1.StepStatus, phase v1alpha1.StepPhase, msg string) {
	st.Phase = phase
	st.Message = msg
	st.FinishedAt = &metav1.Time{Time: e.now()}
}

// failStep finishes a step that failed, preserving the reason code so the
// failure is classifiable and not merely readable.
func (e *Engine) failStep(st *v1alpha1.StepStatus, err error) {
	st.Reason = ReasonOf(err)
	e.finishStep(st, v1alpha1.StepFailed, err.Error())
}

func (e *Engine) fail(status v1alpha1.PassageStatus, msg string) v1alpha1.PassageStatus {
	status.Phase = v1alpha1.PassageFailed
	status.Message = msg
	status.FinishedAt = &metav1.Time{Time: e.now()}
	return status
}

// abort marks the Passage and every unfinished step as aborted. Steps already
// finished keep their outcome: they did happen, and the record should say so.
func (e *Engine) abort(status v1alpha1.PassageStatus) v1alpha1.PassageStatus {
	now := e.now()
	for i := range status.Steps {
		if !status.Steps[i].Phase.Terminal() {
			status.Steps[i].Phase = v1alpha1.StepAborted
			status.Steps[i].FinishedAt = &metav1.Time{Time: now}
		}
	}
	status.Phase = v1alpha1.PassageAborted
	status.Message = "aborted"
	status.FinishedAt = &metav1.Time{Time: now}
	return status
}

// collectOutputs rebuilds the alias->output map from persisted status, so a
// resumed Passage sees the same outputs an uninterrupted one would.
func collectOutputs(steps []v1alpha1.StepStatus) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, st := range steps {
		if st.As == "" || st.Output == nil {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(st.Output.Raw, &v); err == nil {
			out[st.As] = v
		}
	}
	return out
}
