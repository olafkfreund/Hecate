// Package steps holds Hecate's built-in Passage steps.
package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepFluxWait is the value used in `steps[].uses`.
const StepFluxWait = "flux-wait"

// Failure reasons this step can report. Stable codes, not prose: they are what
// `hecate diagnose` and a failure-class dashboard match on.
const (
	// ReasonInvalidConfig means the step's `with:` block is wrong. Retrying
	// cannot help.
	ReasonInvalidConfig = "InvalidConfig"
	// ReasonFluxDegraded means Flux stopped retrying, or has been failing
	// longer than failAfter. Distinct from a wait: this one needs a human.
	ReasonFluxDegraded = "FluxDegraded"
)

// FluxWait blocks a Passage until Flux has applied the state the Passage just
// pushed.
//
// This is the join between Hecate and the delivery engine. Hecate writes to git
// and never speaks to Flux; this step watches Flux's own status to learn
// whether the write took effect. Git is the rendezvous.
type FluxWait struct {
	checker *health.FluxChecker
}

// NewFluxWait returns a flux-wait step backed by the given checker.
func NewFluxWait(c *health.FluxChecker) *FluxWait { return &FluxWait{checker: c} }

// Name implements passage.Runner.
func (f *FluxWait) Name() string { return StepFluxWait }

// Run implements passage.Runner.
func (f *FluxWait) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[health.FluxConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepFluxWait, err)
	}
	if err := cfg.Validate(sc.Namespace, f.checker.AllowCrossNamespace); err != nil {
		// Bad configuration will not become good by waiting. That includes a
		// cross-namespace reference: it is refused, not retried.
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepFluxWait, err)
	}

	status, issues, details := f.checker.Evaluate(ctx, cfg, sc.Namespace)

	// Hand the Gate the same criteria we waited on, so it keeps watching
	// exactly what this step verified. Without it the Gate goes blind the
	// moment the Passage ends.
	watch, err := watchFor(cfg)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepFluxWait, err)
	}

	out := map[string]any{"resources": details}

	switch status {
	case v1alpha1.HealthHealthy:
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("Flux applied the promoted revision to %d resource(s)", len(cfg.Resources)),
			Output:  out,
			Watch:   watch,
		}, nil

	case v1alpha1.HealthDegraded:
		// Flux has stopped retrying, or has been failing past failAfter. More
		// waiting will not help.
		return passage.StepResult{
			Phase:   v1alpha1.StepFailed,
			Message: summarise(issues),
			Output:  out,
		}, passage.FailTerminal(ReasonFluxDegraded, "Flux reconciliation failed: %s", summarise(issues))

	default: // Progressing or Unknown — keep waiting.
		return passage.StepResult{
			Phase:      v1alpha1.StepRunning,
			Message:    summarise(issues),
			Output:     out,
			RetryAfter: 15 * time.Second,
		}, nil
	}
}

// watchFor converts the step config into a Gate health check.
func watchFor(cfg health.FluxConfig) ([]v1alpha1.HealthCheck, error) {
	// The step's own Evaluate above needed an exact revision match to prove
	// this crossing. A standing Gate check does not: git keeps moving after
	// the Passage ends, and a Gate pinned to the crossing's commit would
	// report Progressing forever once a later reconcile lands. cfg is a
	// value here, so this cannot affect the Evaluate call above.
	cfg.ExpectedRevision = ""

	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("cannot encode health check: %w", err)
	}
	return []v1alpha1.HealthCheck{{
		Uses: health.CheckerFlux,
		With: &apiextensionsv1.JSON{Raw: encoded},
	}}, nil
}

// summarise reduces a list of issues to one line for status display.
func summarise(issues []string) string {
	switch len(issues) {
	case 0:
		return "waiting for Flux"
	case 1:
		return issues[0]
	default:
		return fmt.Sprintf("%s (and %d more)", issues[0], len(issues)-1)
	}
}
