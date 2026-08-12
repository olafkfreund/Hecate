// Package passage runs the steps that move a Bundle through a Gate.
//
// A step is invoked repeatedly rather than blocking: it reports what it sees
// and returns, and the engine calls it again later if it is still waiting. That
// keeps long waits (a Flux reconciliation, a pull request review) out of
// goroutines and in the Passage's persisted status, so a controller restart
// resumes rather than restarts.
package passage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Runner implements one kind of step.
type Runner interface {
	// Name is the value used in `steps[].uses`.
	Name() string
	// Run performs one invocation. It must return promptly: to wait, return
	// StepRunning with a RetryAfter rather than sleeping.
	Run(ctx context.Context, sc *StepContext) (StepResult, error)
}

// StepContext is everything a step is given.
type StepContext struct {
	// Namespace is where the Gate and Bundle live.
	Namespace string
	// Gate being crossed.
	Gate string
	// Passage performing the crossing.
	Passage string
	// Bundle being moved. Steps read artifact versions from here.
	Bundle *v1alpha1.Bundle
	// Actor initiated the Passage. ActorController when nobody did.
	Actor string
	// WorkDir is a scratch directory shared by every step in this Passage —
	// where one step clones a repo and a later one commits it.
	WorkDir string
	// Config is this step's `with:` block.
	Config json.RawMessage
	// Outputs holds prior steps' outputs, keyed by their `as:` alias.
	Outputs map[string]map[string]any
	// Attempt is how many times this step has already run, starting at 0.
	Attempt int32
	// Failed reports that an earlier step has already failed the Passage.
	//
	// A step only sees this if it asked to run anyway (`if: failed` or
	// `if: always`) — otherwise it is skipped and never invoked. It is here so
	// a step reporting the outcome can report the real one: the engine knows,
	// and making the user restate it in configuration would be a second place
	// for the truth to live (D46).
	Failed bool
	// Traceparent is the W3C trace context for this crossing, or empty when
	// tracing is off. Steps that write something durable should carry it, so a
	// promotion can be correlated end to end (D42).
	Traceparent string
	// StartedAt is when the Passage began.
	//
	// Steps that produce content should derive timestamps from this rather than
	// the wall clock, so re-running a Passage yields byte-identical output. A
	// commit stamped with time.Now() gets a new SHA on every attempt, which
	// turns a harmless retry into a second commit on the branch.
	StartedAt time.Time
}

// DecodeConfig unmarshals a step's config into a typed struct.
func DecodeConfig[T any](sc *StepContext) (T, error) {
	return CheckConfig[T](sc.Config)
}

// CheckConfig decodes a `with:` block strictly, and is how a step validates one
// without running.
//
// Strict because the default is silent: json.Unmarshal drops fields it does not
// recognise, so `mesage:` for `message:` decodes cleanly into an empty struct
// and the step fails later complaining the message is missing — pointing at the
// field that is there rather than the one that is misspelt. Refusing the unknown
// field names the actual mistake.
func CheckConfig[T any](raw json.RawMessage) (T, error) {
	var cfg T
	if len(raw) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// StepResult is the outcome of one invocation.
type StepResult struct {
	// Phase is the outcome. StepRunning means "call me again".
	Phase v1alpha1.StepPhase
	// Message explains the phase, and is shown in the UI and CLI while waiting.
	Message string
	// Output is exposed to later steps under this step's alias.
	Output map[string]any
	// Watch are health checks this step hands to the Gate, so the Gate keeps
	// monitoring what the step waited for. Without this a Gate goes blind the
	// moment its Passage finishes.
	Watch []v1alpha1.HealthCheck
	// Evidence is the compliance record this step produced or consulted, copied
	// onto the Passage's status.
	//
	// Carried here rather than dug out of Output by the controller, for the same
	// reason as Watch: a step knows what it recorded, and a controller matching
	// on well-known output keys would break the moment a step chose a different
	// name for them.
	Evidence *v1alpha1.EvidenceRef
	// RetryAfter suggests how long to wait before the next invocation.
	RetryAfter time.Duration
}

// StepError is a step failure with a machine-readable reason.
//
// The reason is what makes a failure something other than prose. `hecate
// diagnose`, a dashboard counting failure classes, and anything reasoning over
// a stuck Passage all need to distinguish "the git host rejected our
// credentials" from "Flux gave up" without parsing English.
//
// Structured detail belongs in the step's Output, not here: the engine already
// records Output on failure, and a second place to put facts would only invite
// the two to disagree.
type StepError struct {
	// Reason is a stable, machine-readable code in PascalCase, following the
	// convention Kubernetes uses for condition reasons — GitAuthFailed,
	// FluxStalled, InvalidConfig. Stable is the operative word: it is a
	// contract, so renaming one breaks whatever was matching on it.
	//
	// Deliberately not a closed enum. Each step names its own failures; a
	// central registry would be a bottleneck and a merge conflict, and steps
	// live in different packages.
	Reason string
	// Terminal marks a failure not worth retrying — bad configuration, a
	// missing reference, anything that will fail identically next time.
	Terminal bool
	Err      error
}

func (e *StepError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Err.Error()
}

func (e *StepError) Unwrap() error { return e.Err }

// Fail builds a retryable step failure.
func Fail(reason, format string, args ...any) error {
	return &StepError{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// FailTerminal builds a step failure that must not be retried.
func FailTerminal(reason, format string, args ...any) error {
	return &StepError{Reason: reason, Terminal: true, Err: fmt.Errorf(format, args...)}
}

// Terminalf builds a terminal failure without a reason code.
//
// Kept for the cases where no useful code exists, but prefer FailTerminal:
// a failure with no reason is one nothing downstream can act on.
func Terminalf(format string, args ...any) error {
	return &StepError{Reason: ReasonUnknown, Terminal: true, Err: fmt.Errorf(format, args...)}
}

// ReasonUnknown is used when a step fails without naming a reason.
const ReasonUnknown = "Unknown"

// IsTerminal reports whether an error should stop retrying.
func IsTerminal(err error) bool {
	var e *StepError
	return errors.As(err, &e) && e.Terminal
}

// ReasonOf extracts a failure's reason code, or ReasonUnknown.
func ReasonOf(err error) string {
	var e *StepError
	if errors.As(err, &e) && e.Reason != "" {
		return e.Reason
	}
	if err != nil {
		return ReasonUnknown
	}
	return ""
}

// Registry holds the available step Runners.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]Runner
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{runners: map[string]Runner{}} }

// Register adds a Runner. A duplicate name is an error, not a silent
// overwrite — two runners claiming one name would fail confusingly at runtime.
func (r *Registry) Register(run Runner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runners[run.Name()]; exists {
		return fmt.Errorf("step %q is already registered", run.Name())
	}
	r.runners[run.Name()] = run
	return nil
}

// MustRegister is Register, panicking on error. For startup wiring.
func (r *Registry) MustRegister(run Runner) {
	if err := r.Register(run); err != nil {
		panic(err)
	}
}

// Get returns the Runner registered under name.
func (r *Registry) Get(name string) (Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runners[name]
	return run, ok
}

// Names lists the registered steps, sorted.
// ConfigChecker is implemented by a step that can judge its `with:` block
// without running.
//
// Optional: a step that does not implement it is simply not checked, which is
// better than a registry that refuses to hold steps nobody has got to yet.
type ConfigChecker interface {
	CheckConfig(raw json.RawMessage) error
}

// StepProblem is one thing wrong with a Gate's step list.
type StepProblem struct {
	// Index is the step's position, which is how an author finds it: steps have
	// no required name, so "step 3" is often the only way to point at one.
	Index int
	Uses  string
	Err   error
}

func (p StepProblem) Error() string {
	where := fmt.Sprintf("steps[%d]", p.Index)
	if p.Uses != "" {
		where += " (" + p.Uses + ")"
	}
	return where + ": " + p.Err.Error()
}

// Validate reports everything wrong with a step list.
//
// Every problem, not the first: an author fixing a Gate wants the whole list,
// and returning one at a time turns a single mistake into several apply-and-
// wait cycles.
func (r *Registry) Validate(steps []v1alpha1.Step) []StepProblem {
	var problems []StepProblem
	seen := map[string]int{}

	for i, step := range steps {
		if step.Uses == "" {
			problems = append(problems, StepProblem{Index: i,
				Err: errors.New("no step named — `uses` is required")})
			continue
		}

		// An alias is how later steps read this one's output, so a duplicate
		// silently shadows the earlier step's values.
		if step.As != "" {
			if first, dup := seen[step.As]; dup {
				problems = append(problems, StepProblem{Index: i, Uses: step.Uses,
					Err: fmt.Errorf("alias %q is already used by steps[%d]", step.As, first)})
			} else {
				seen[step.As] = i
			}
		}

		runner, ok := r.Get(step.Uses)
		if !ok {
			problems = append(problems, StepProblem{Index: i, Uses: step.Uses,
				Err: fmt.Errorf("no such step (available: %s)", strings.Join(r.Names(), ", "))})
			continue
		}

		checker, ok := runner.(ConfigChecker)
		if !ok {
			continue
		}
		var raw json.RawMessage
		if step.With != nil {
			raw = step.With.Raw
		}
		if err := checker.CheckConfig(raw); err != nil {
			problems = append(problems, StepProblem{Index: i, Uses: step.Uses, Err: err})
		}
	}
	return problems
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runners))
	for n := range r.runners {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ActorController is what a crossing the controller started records as having
// asked for it.
//
// Worth distinguishing from a person: an automatic crossing has no human
// deployer, and reporting a robot as one to a system evaluating segregation of
// duties would make four-eyes pass with three roles and two humans.
const ActorController = "controller"
