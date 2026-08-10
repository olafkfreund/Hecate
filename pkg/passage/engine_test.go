package passage

import (
	"context"
	"errors"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var clock = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// scripted is a Runner that returns a canned sequence of results, one per
// invocation, so multi-tick behaviour can be tested without any real waiting.
type scripted struct {
	name    string
	results []StepResult
	errs    []error
	calls   int
}

func (s *scripted) Name() string { return s.name }

func (s *scripted) Run(context.Context, *StepContext) (StepResult, error) {
	i := s.calls
	s.calls++
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.results[i], err
}

func ok(msg string) StepResult {
	return StepResult{Phase: v1alpha1.StepSucceeded, Message: msg}
}

func newEngine(runners ...Runner) *Engine {
	reg := NewRegistry()
	for _, r := range runners {
		reg.MustRegister(r)
	}
	return &Engine{Registry: reg, Now: func() time.Time { return clock }}
}

func passageWith(steps ...v1alpha1.Step) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "b1", Steps: steps},
	}
}

func TestAdvanceRunsAllStepsToSuccess(t *testing.T) {
	a := &scripted{name: "a", results: []StepResult{ok("did a")}}
	b := &scripted{name: "b", results: []StepResult{ok("did b")}}
	e := newEngine(a, b)

	out := e.Advance(context.Background(), passageWith(
		v1alpha1.Step{Uses: "a"}, v1alpha1.Step{Uses: "b"},
	), nil)

	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded (%s)", out.Status.Phase, out.Status.Message)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("each step should run once, got a=%d b=%d", a.calls, b.calls)
	}
	if out.Status.FinishedAt == nil {
		t.Error("a finished Passage must record FinishedAt")
	}
}

func TestAdvanceWaitsOnRunningStepAndResumes(t *testing.T) {
	// The step is still waiting the first time, done the second.
	waiter := &scripted{name: "wait", results: []StepResult{
		{Phase: v1alpha1.StepRunning, Message: "waiting for Flux", RetryAfter: 5 * time.Second},
		ok("converged"),
	}}
	after := &scripted{name: "after", results: []StepResult{ok("done")}}
	e := newEngine(waiter, after)
	p := passageWith(v1alpha1.Step{Uses: "wait"}, v1alpha1.Step{Uses: "after"})

	out := e.Advance(context.Background(), p, nil)
	if out.Status.Phase != v1alpha1.PassageRunning {
		t.Fatalf("phase = %s, want Running", out.Status.Phase)
	}
	if out.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %s, want 5s", out.RequeueAfter)
	}
	if after.calls != 0 {
		t.Error("a later step must not run while an earlier one is still waiting")
	}

	// Persist the status the way the controller would, then resume.
	p.Status = out.Status
	out = e.Advance(context.Background(), p, nil)
	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded (%s)", out.Status.Phase, out.Status.Message)
	}
	if waiter.calls != 2 {
		t.Errorf("waiting step should be invoked twice, got %d", waiter.calls)
	}
	if out.Status.Steps[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", out.Status.Steps[0].Attempts)
	}
}

func TestAdvanceStopsAtFailure(t *testing.T) {
	bad := &scripted{
		name:    "bad",
		results: []StepResult{{Phase: v1alpha1.StepFailed}},
		errs:    []error{errors.New("boom")},
	}
	never := &scripted{name: "never", results: []StepResult{ok("")}}
	e := newEngine(bad, never)

	out := e.Advance(context.Background(), passageWith(
		v1alpha1.Step{Uses: "bad"}, v1alpha1.Step{Uses: "never"},
	), nil)

	if out.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", out.Status.Phase)
	}
	if never.calls != 0 {
		t.Error("steps after a failure must not run")
	}
	if out.Status.Message != "boom" {
		t.Errorf("message = %q, want the step's error", out.Status.Message)
	}
}

func TestContinueOnErrorProceeds(t *testing.T) {
	flaky := &scripted{
		name:    "flaky",
		results: []StepResult{{Phase: v1alpha1.StepFailed}},
		errs:    []error{errors.New("ignore me")},
	}
	next := &scripted{name: "next", results: []StepResult{ok("ran anyway")}}
	e := newEngine(flaky, next)

	out := e.Advance(context.Background(), passageWith(
		v1alpha1.Step{Uses: "flaky", ContinueOnError: true}, v1alpha1.Step{Uses: "next"},
	), nil)

	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded", out.Status.Phase)
	}
	if next.calls != 1 {
		t.Error("the next step should have run")
	}
	// The failure is still recorded — continueOnError forgives, it does not hide.
	if out.Status.Steps[0].Phase != v1alpha1.StepFailed {
		t.Errorf("step 0 phase = %s, want Failed", out.Status.Steps[0].Phase)
	}
}

func TestTerminalErrorIgnoresContinueOnError(t *testing.T) {
	// continueOnError forgives transient faults, not bad configuration.
	broken := &scripted{
		name:    "broken",
		results: []StepResult{{}},
		errs:    []error{Terminalf("bad config")},
	}
	next := &scripted{name: "next", results: []StepResult{ok("")}}
	e := newEngine(broken, next)

	out := e.Advance(context.Background(), passageWith(
		v1alpha1.Step{Uses: "broken", ContinueOnError: true}, v1alpha1.Step{Uses: "next"},
	), nil)

	if out.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", out.Status.Phase)
	}
	if next.calls != 0 {
		t.Error("a terminal error must stop the Passage even with continueOnError")
	}
}

func TestUnknownStepFailsWithAvailableNames(t *testing.T) {
	e := newEngine(&scripted{name: "known", results: []StepResult{ok("")}})

	out := e.Advance(context.Background(), passageWith(v1alpha1.Step{Uses: "typo"}), nil)

	if out.Status.Phase != v1alpha1.PassageFailed {
		t.Fatalf("phase = %s, want Failed", out.Status.Phase)
	}
	if !contains(out.Status.Message, "known") {
		t.Errorf("the error should list available steps, got %q", out.Status.Message)
	}
}

func TestOutputsFlowBetweenSteps(t *testing.T) {
	producer := &scripted{name: "producer", results: []StepResult{
		{Phase: v1alpha1.StepSucceeded, Output: map[string]any{"sha": "9f8c1a2b"}},
	}}
	var seen map[string]map[string]any
	consumer := runnerFunc{name: "consumer", fn: func(_ context.Context, sc *StepContext) (StepResult, error) {
		seen = sc.Outputs
		return ok(""), nil
	}}
	e := newEngine(producer, consumer)

	e.Advance(context.Background(), passageWith(
		v1alpha1.Step{Uses: "producer", As: "commit"}, v1alpha1.Step{Uses: "consumer"},
	), nil)

	if seen["commit"]["sha"] != "9f8c1a2b" {
		t.Fatalf("consumer saw outputs %v, want commit.sha=9f8c1a2b", seen)
	}
}

// Outputs must survive a controller restart, since they live in the Passage.
func TestOutputsSurviveResume(t *testing.T) {
	var seen map[string]map[string]any
	consumer := runnerFunc{name: "consumer", fn: func(_ context.Context, sc *StepContext) (StepResult, error) {
		seen = sc.Outputs
		return ok(""), nil
	}}
	e := newEngine(consumer)

	p := passageWith(v1alpha1.Step{Uses: "gone", As: "commit"}, v1alpha1.Step{Uses: "consumer"})
	// Simulate a previously-persisted status: step 0 already finished.
	p.Status = v1alpha1.PassageStatus{
		Phase:       v1alpha1.PassageRunning,
		CurrentStep: 1,
		Steps: []v1alpha1.StepStatus{
			{Uses: "gone", As: "commit", Phase: v1alpha1.StepSucceeded,
				Output: &apiextensionsv1.JSON{Raw: []byte(`{"sha":"deadbeef"}`)}},
			{Uses: "consumer", Phase: v1alpha1.StepPending},
		},
	}

	out := e.Advance(context.Background(), p, nil)
	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s, want Succeeded (%s)", out.Status.Phase, out.Status.Message)
	}
	if seen["commit"]["sha"] != "deadbeef" {
		t.Errorf("resumed Passage lost earlier outputs: %v", seen)
	}
}

func TestAbortStopsAndMarksUnfinishedSteps(t *testing.T) {
	e := newEngine(&scripted{name: "a", results: []StepResult{ok("")}})
	p := passageWith(v1alpha1.Step{Uses: "a"}, v1alpha1.Step{Uses: "a"})
	p.Spec.Abort = true
	p.Status = v1alpha1.PassageStatus{
		Phase: v1alpha1.PassageRunning,
		Steps: []v1alpha1.StepStatus{
			{Uses: "a", Phase: v1alpha1.StepSucceeded},
			{Uses: "a", Phase: v1alpha1.StepRunning},
		},
	}

	out := e.Advance(context.Background(), p, nil)

	if out.Status.Phase != v1alpha1.PassageAborted {
		t.Fatalf("phase = %s, want Aborted", out.Status.Phase)
	}
	// A step that already finished keeps its outcome: it did happen.
	if out.Status.Steps[0].Phase != v1alpha1.StepSucceeded {
		t.Errorf("finished step was overwritten: %s", out.Status.Steps[0].Phase)
	}
	if out.Status.Steps[1].Phase != v1alpha1.StepAborted {
		t.Errorf("unfinished step = %s, want Aborted", out.Status.Steps[1].Phase)
	}
}

func TestTerminalPassageIsNotRerun(t *testing.T) {
	r := &scripted{name: "a", results: []StepResult{ok("")}}
	e := newEngine(r)
	p := passageWith(v1alpha1.Step{Uses: "a"})
	p.Status = v1alpha1.PassageStatus{Phase: v1alpha1.PassageSucceeded}

	out := e.Advance(context.Background(), p, nil)
	if r.calls != 0 {
		t.Error("a completed Passage must never run its steps again")
	}
	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Errorf("phase changed to %s", out.Status.Phase)
	}
}

func TestWatchIsCollectedFromSteps(t *testing.T) {
	emitter := &scripted{name: "emit", results: []StepResult{{
		Phase: v1alpha1.StepSucceeded,
		Watch: []v1alpha1.HealthCheck{{Uses: "flux"}},
	}}}
	e := newEngine(emitter)

	out := e.Advance(context.Background(), passageWith(v1alpha1.Step{Uses: "emit"}), nil)
	if len(out.Watch) != 1 || out.Watch[0].Uses != "flux" {
		t.Fatalf("watch = %v, want one flux check for the Gate to adopt", out.Watch)
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&scripted{name: "dup"}); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := reg.Register(&scripted{name: "dup"}); err == nil {
		t.Error("registering a duplicate name should fail, not silently overwrite")
	}
}

// runnerFunc adapts a function to the Runner interface.
type runnerFunc struct {
	name string
	fn   func(context.Context, *StepContext) (StepResult, error)
}

func (r runnerFunc) Name() string { return r.name }
func (r runnerFunc) Run(ctx context.Context, sc *StepContext) (StepResult, error) {
	return r.fn(ctx, sc)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
