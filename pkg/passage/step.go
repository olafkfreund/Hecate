// Package passage runs the steps that move a Bundle through a Gate.
//
// A step is invoked repeatedly rather than blocking: it reports what it sees
// and returns, and the engine calls it again later if it is still waiting. That
// keeps long waits (a Flux reconciliation, a pull request review) out of
// goroutines and in the Passage's persisted status, so a controller restart
// resumes rather than restarts.
package passage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	// Actor initiated the Passage.
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
}

// DecodeConfig unmarshals a step's config into a typed struct.
func DecodeConfig[T any](sc *StepContext) (T, error) {
	var cfg T
	if len(sc.Config) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(sc.Config, &cfg); err != nil {
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
	// RetryAfter suggests how long to wait before the next invocation.
	RetryAfter time.Duration
}

// Terminal marks an error as not worth retrying — bad configuration, a missing
// reference, anything that will fail identically next time.
type Terminal struct{ Err error }

func (t *Terminal) Error() string {
	if t.Err == nil {
		return "terminal error"
	}
	return t.Err.Error()
}

func (t *Terminal) Unwrap() error { return t.Err }

// Terminalf builds a Terminal error.
func Terminalf(format string, args ...any) error {
	return &Terminal{Err: fmt.Errorf(format, args...)}
}

// IsTerminal reports whether an error should stop retrying.
func IsTerminal(err error) bool {
	var t *Terminal
	return errors.As(err, &t)
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
