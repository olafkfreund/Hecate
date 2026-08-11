package passage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
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

// ReasonUnregisteredStep is set when a Passage names a step that does not exist.
const ReasonUnregisteredStep = "UnregisteredStep"

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
		return Outcome{Status: e.abort(status, now)}
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

	for i := int(status.CurrentStep); i < len(p.Spec.Steps); i++ {
		step := p.Spec.Steps[i]
		st := &status.Steps[i]
		status.CurrentStep = int32(i)

		if st.Phase.Terminal() {
			continue
		}

		runner, ok := e.Registry.Get(step.Uses)
		if !ok {
			// An unknown step is a configuration error, not a transient fault.
			st.Reason = ReasonUnregisteredStep
			e.finishStep(st, v1alpha1.StepFailed, fmt.Sprintf(
				"no step named %q is registered (available: %v)", step.Uses, e.Registry.Names()), now)
			return Outcome{Status: e.fail(status, st.Message, now), Watch: watch}
		}

		if st.StartedAt == nil {
			st.StartedAt = &metav1.Time{Time: now}
		}
		st.Phase = v1alpha1.StepRunning
		st.Attempts++

		var cfg json.RawMessage
		if step.With != nil {
			cfg = step.With.Raw
		}

		res, err := runner.Run(ctx, &StepContext{
			Namespace: p.Namespace,
			Gate:      p.Spec.Gate,
			Passage:   p.Name,
			Bundle:    bundle,
			Actor:     p.Spec.Actor,
			WorkDir:   workDir,
			Config:    cfg,
			Outputs:   outputs,
			Attempt:   st.Attempts - 1,
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

		switch {
		case err != nil && (IsTerminal(err) || !step.ContinueOnError):
			e.failStep(st, err, now)
			if step.ContinueOnError && !IsTerminal(err) {
				continue
			}
			return Outcome{Status: e.fail(status, err.Error(), now), Watch: watch}

		case err != nil: // non-terminal, and the step is allowed to fail
			e.failStep(st, err, now)
			continue

		case res.Phase == v1alpha1.StepRunning:
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
			e.finishStep(st, phase, res.Message, now)
			if phase == v1alpha1.StepFailed && !step.ContinueOnError {
				return Outcome{Status: e.fail(status, res.Message, now), Watch: watch}
			}
		}
	}

	status.Phase = v1alpha1.PassageSucceeded
	status.Message = fmt.Sprintf("crossed %s", p.Spec.Gate)
	status.FinishedAt = &metav1.Time{Time: now}
	return Outcome{Status: status, Watch: watch}
}

func (e *Engine) finishStep(st *v1alpha1.StepStatus, phase v1alpha1.StepPhase, msg string, now time.Time) {
	st.Phase = phase
	st.Message = msg
	st.FinishedAt = &metav1.Time{Time: now}
}

// failStep finishes a step that failed, preserving the reason code so the
// failure is classifiable and not merely readable.
func (e *Engine) failStep(st *v1alpha1.StepStatus, err error, now time.Time) {
	st.Reason = ReasonOf(err)
	e.finishStep(st, v1alpha1.StepFailed, err.Error(), now)
}

func (e *Engine) fail(status v1alpha1.PassageStatus, msg string, now time.Time) v1alpha1.PassageStatus {
	status.Phase = v1alpha1.PassageFailed
	status.Message = msg
	status.FinishedAt = &metav1.Time{Time: now}
	return status
}

// abort marks the Passage and every unfinished step as aborted. Steps already
// finished keep their outcome: they did happen, and the record should say so.
func (e *Engine) abort(status v1alpha1.PassageStatus, now time.Time) v1alpha1.PassageStatus {
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
