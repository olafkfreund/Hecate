// Package health assesses whether what a Gate holds is actually working.
//
// Health is continuous and answers "is it running right now?". It is distinct
// from verification, which is one-shot and answers "did it work?". A Gate can
// be healthy and unverified (deployed, tests not yet run) or verified and
// unhealthy (it passed, then fell over an hour later). Conflating them is how
// you end up promoting from an environment that is currently on fire.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Checker assesses one kind of thing. Implementations are registered by name
// and selected by a Gate's `watch[].uses`.
type Checker interface {
	// Name is the value used in `watch[].uses`.
	Name() string
	// Check assesses the target described by cfg. It must not block for long:
	// the caller polls, so a Checker reports what it sees now and returns.
	Check(ctx context.Context, req Request) v1alpha1.HealthReport
}

// Request is what a Checker is asked to assess.
type Request struct {
	// Namespace is where the Gate lives, and the default namespace for any
	// resource the check refers to without one.
	Namespace string
	// Gate is the Gate being assessed, for logging and error messages.
	Gate string
	// Config is the checker-specific configuration from `watch[].with`.
	Config json.RawMessage
}

// DecodeConfig unmarshals a Request's config into a typed struct.
func DecodeConfig[T any](req Request) (T, error) {
	var cfg T
	if len(req.Config) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(req.Config, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// Unknown builds a report for a check that could not be performed. Used for
// configuration errors, where reporting Degraded would wrongly suggest we
// looked at the workload and found it broken.
func Unknown(format string, args ...any) v1alpha1.HealthReport {
	return v1alpha1.HealthReport{
		Status: v1alpha1.HealthUnknown,
		Issues: []string{fmt.Sprintf(format, args...)},
	}
}

// Registry holds the available Checkers.
type Registry struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{checkers: map[string]Checker{}}
}

// Register adds a Checker. Registering the same name twice is an error rather
// than a silent overwrite — a duplicate name means two checkers disagree about
// who owns it, and the loser would fail confusingly at runtime.
func (r *Registry) Register(c Checker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.checkers[c.Name()]; exists {
		return fmt.Errorf("health checker %q is already registered", c.Name())
	}
	r.checkers[c.Name()] = c
	return nil
}

// MustRegister is Register, panicking on error. For use in wiring code at
// startup, where a duplicate is a programming error.
func (r *Registry) MustRegister(c Checker) {
	if err := r.Register(c); err != nil {
		panic(err)
	}
}

// Get returns the Checker registered under name.
func (r *Registry) Get(name string) (Checker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.checkers[name]
	return c, ok
}

// Names lists the registered checkers, sorted. For error messages and `hecate
// explain`.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.checkers))
	for n := range r.checkers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Assess runs every check a Gate declares and reduces them to one report.
//
// An unknown checker name is reported rather than skipped: silently ignoring a
// check the user asked for would make a Gate look healthier than it is, which
// is the exact failure mode this package exists to prevent.
func (r *Registry) Assess(ctx context.Context, namespace, gate string, checks []v1alpha1.HealthCheck) v1alpha1.HealthReport {
	if len(checks) == 0 {
		return v1alpha1.HealthReport{Status: v1alpha1.HealthNotApplicable}
	}

	overall := v1alpha1.HealthHealthy
	var issues []string
	details := map[string]any{}

	for _, check := range checks {
		checker, ok := r.Get(check.Uses)
		if !ok {
			overall = overall.Merge(v1alpha1.HealthUnknown)
			issues = append(issues, fmt.Sprintf(
				"no health checker named %q is registered (available: %v)", check.Uses, r.Names(),
			))
			continue
		}

		var raw json.RawMessage
		if check.With != nil {
			raw = check.With.Raw
		}

		report := checker.Check(ctx, Request{Namespace: namespace, Gate: gate, Config: raw})
		overall = overall.Merge(report.Status)
		issues = append(issues, report.Issues...)
		if report.Details != nil {
			var d any
			if err := json.Unmarshal(report.Details.Raw, &d); err == nil {
				details[check.Uses] = d
			}
		}
	}

	out := v1alpha1.HealthReport{Status: overall, Issues: issues}
	if len(details) > 0 {
		if encoded, err := json.Marshal(details); err == nil {
			out.Details = &apiextensionsv1.JSON{Raw: encoded}
		}
	}
	return out
}
