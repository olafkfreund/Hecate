// Package kargo adapts Hecate's Flux status evaluation to Kargo's health
// checker and promotion step interfaces.
//
// This is the whole of Path A: Kargo supplies the pipeline model, the other 34
// engine-neutral promotion steps, the API server, RBAC, SSO, and sharding;
// Hecate supplies the delivery-engine adapter. Everything below is glue — the
// judgement lives in package flux, which does not import Kargo.
package kargo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/pkg/health"
	"github.com/akuity/kargo/pkg/promotion"

	"github.com/olafkfreund/hecate/pkg/flux"
)

const (
	// StepKindFluxWait is the name of the promotion step that blocks until Flux
	// has applied the promoted revision.
	StepKindFluxWait = "flux-wait"
	// CheckerKindFlux is the name of the Stage health checker.
	CheckerKindFlux = "flux"
)

// defaultAPIVersions maps the Flux kinds we understand to their current API
// version, so users need only write `kind: Kustomization`. An explicit
// apiVersion in the config always wins, which is the escape hatch for kinds we
// have not enumerated and for future API bumps.
var defaultAPIVersions = map[string]string{
	"Kustomization":  "kustomize.toolkit.fluxcd.io/v1",
	"HelmRelease":    "helm.toolkit.fluxcd.io/v2",
	"GitRepository":  "source.toolkit.fluxcd.io/v1",
	"OCIRepository":  "source.toolkit.fluxcd.io/v1",
	"HelmChart":      "source.toolkit.fluxcd.io/v1",
	"HelmRepository": "source.toolkit.fluxcd.io/v1",
	"Bucket":         "source.toolkit.fluxcd.io/v1",
}

// ResourceRef identifies one Flux resource to watch.
type ResourceRef struct {
	Kind string `json:"kind"`
	// APIVersion overrides the default for Kind. Required for kinds not in
	// defaultAPIVersions.
	APIVersion string `json:"apiVersion,omitempty"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// Config is the shared configuration of the flux-wait step and the flux health
// checker. The step emits it verbatim as the checker's input, so a Stage gets
// continuous health on exactly what the promotion waited for.
type Config struct {
	Resources []ResourceRef `json:"resources"`
	// ExpectedRevision, when set, requires Flux to have applied this revision.
	// Typically wired from an upstream git-commit step:
	//   expectedRevision: ${{ outputs.commit.commit }}
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	// FailAfter is a Go duration; how long a resource may be un-Ready before it
	// is reported Unhealthy rather than Progressing. Defaults to 10m.
	FailAfter string `json:"failAfter,omitempty"`
}

func (c Config) failAfter() (time.Duration, error) {
	if c.FailAfter == "" {
		return flux.DefaultFailAfter, nil
	}
	d, err := time.ParseDuration(c.FailAfter)
	if err != nil {
		return 0, fmt.Errorf("invalid failAfter %q: %w", c.FailAfter, err)
	}
	return d, nil
}

func (r ResourceRef) gvk() (schema.GroupVersionKind, error) {
	av := r.APIVersion
	if av == "" {
		var ok bool
		if av, ok = defaultAPIVersions[r.Kind]; !ok {
			return schema.GroupVersionKind{}, fmt.Errorf(
				"no default apiVersion for kind %q; set apiVersion explicitly", r.Kind,
			)
		}
	}
	gv, err := schema.ParseGroupVersion(av)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid apiVersion %q: %w", av, err)
	}
	return gv.WithKind(r.Kind), nil
}

func (c Config) validate() error {
	if len(c.Resources) == 0 {
		return fmt.Errorf("at least one resource is required")
	}
	for i, r := range c.Resources {
		if r.Name == "" {
			return fmt.Errorf("resources[%d]: name is required", i)
		}
		if _, err := r.gvk(); err != nil {
			return fmt.Errorf("resources[%d]: %w", i, err)
		}
	}
	_, err := c.failAfter()
	return err
}

// evaluate resolves every configured resource and reduces them to one state.
// A resource that cannot be read at all is reported rather than swallowed:
// a health check that silently ignores a missing Kustomization is worse than
// no health check.
func (c Config) evaluate(ctx context.Context, cl client.Client, defaultNS string) (flux.State, []string, map[string]any) {
	failAfter, _ := c.failAfter() // already validated
	state := flux.StateHealthy
	var issues []string
	details := make(map[string]any, len(c.Resources))

	for _, ref := range c.Resources {
		ns := ref.Namespace
		if ns == "" {
			ns = defaultNS
		}
		gvk, _ := ref.gvk() // already validated

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)

		var res flux.Result
		if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
			res = flux.Result{
				State:  flux.StateUnknown,
				Issues: []string{fmt.Sprintf("could not read %s %s/%s: %s", ref.Kind, ns, ref.Name, err)},
			}
		} else {
			res = flux.Evaluate(obj, flux.Options{
				ExpectedRevision: c.ExpectedRevision,
				FailAfter:        failAfter,
			})
		}

		state = state.Merge(res.State)
		issues = append(issues, res.Issues...)
		details[fmt.Sprintf("%s/%s/%s", ref.Kind, ns, ref.Name)] = map[string]any{
			"state":    string(res.State),
			"revision": res.Revision,
		}
	}
	return state, issues, details
}

// ---------------------------------------------------------------------------
// Health checker
// ---------------------------------------------------------------------------

type fluxChecker struct {
	cl client.Client
}

// NewChecker returns a health.Checker that reports the health of Flux
// resources. It is the Flux-native counterpart to Kargo's built-in Argo CD
// checker.
func NewChecker(cl client.Client) health.Checker { return &fluxChecker{cl: cl} }

func (f *fluxChecker) Name() string { return CheckerKindFlux }

func (f *fluxChecker) Check(ctx context.Context, project, _ string, criteria health.Criteria) health.Result {
	cfg, err := health.InputToStruct[Config](criteria.Input)
	if err != nil {
		return health.Result{
			Status: kargoapi.HealthStateUnknown,
			Issues: []string{fmt.Sprintf("invalid %s health check input: %s", f.Name(), err)},
		}
	}
	if err := cfg.validate(); err != nil {
		return health.Result{
			Status: kargoapi.HealthStateUnknown,
			Issues: []string{fmt.Sprintf("invalid %s health check input: %s", f.Name(), err)},
		}
	}

	state, issues, details := cfg.evaluate(ctx, f.cl, project)
	return health.Result{
		Status: toHealthState(state),
		Issues: issues,
		Output: map[string]any{"resources": details},
	}
}

func toHealthState(s flux.State) kargoapi.HealthState {
	switch s {
	case flux.StateHealthy:
		return kargoapi.HealthStateHealthy
	case flux.StateProgressing:
		return kargoapi.HealthStateProgressing
	case flux.StateUnhealthy:
		return kargoapi.HealthStateUnhealthy
	default:
		return kargoapi.HealthStateUnknown
	}
}

// ---------------------------------------------------------------------------
// flux-wait promotion step
// ---------------------------------------------------------------------------

type fluxWaiter struct {
	cl client.Client
}

func newFluxWaiter(caps promotion.StepRunnerCapabilities) promotion.StepRunner {
	// ponytail: KargoClient, not a dedicated delivery-engine client. Kargo's
	// capability bundle only offers KargoClient and ArgoCDClient, so this
	// assumes Flux runs in the same cluster as the Hecate controller — the
	// common case. Remote Flux clusters need an upstream PR adding a generic
	// client to StepRunnerCapabilities; tracked as its own issue.
	return &fluxWaiter{cl: caps.KargoClient}
}

func (f *fluxWaiter) Run(ctx context.Context, stepCtx *promotion.StepContext) (promotion.StepResult, error) {
	cfg, err := promotion.ConfigToStruct[Config](stepCtx.Config)
	if err != nil {
		return errored(), &promotion.TerminalError{
			Err: fmt.Errorf("could not decode %s config: %w", StepKindFluxWait, err),
		}
	}
	if err := cfg.validate(); err != nil {
		// Bad config will never become good by retrying.
		return errored(), &promotion.TerminalError{
			Err: fmt.Errorf("invalid %s config: %w", StepKindFluxWait, err),
		}
	}

	state, issues, details := cfg.evaluate(ctx, f.cl, stepCtx.Project)

	// Hand the Stage the same criteria we just waited on, so its continuous
	// health check watches exactly what the promotion verified.
	hc, hcErr := cfg.criteria()
	if hcErr != nil {
		return errored(), &promotion.TerminalError{Err: hcErr}
	}

	switch state {
	case flux.StateHealthy:
		return promotion.StepResult{
			Status:      kargoapi.PromotionStepStatusSucceeded,
			Message:     fmt.Sprintf("Flux applied the promoted revision to %d resource(s)", len(cfg.Resources)),
			Output:      map[string]any{"resources": details},
			HealthCheck: hc,
		}, nil

	case flux.StateUnhealthy:
		// Flux has stopped retrying, or has been failing past failAfter. More
		// waiting will not help, so fail the promotion rather than spin.
		return promotion.StepResult{
				Status:  kargoapi.PromotionStepStatusFailed,
				Message: joinIssues(issues),
				Output:  map[string]any{"resources": details},
			}, &promotion.TerminalError{
				Err: fmt.Errorf("Flux reconciliation failed: %s", joinIssues(issues)),
			}

	default: // Progressing or Unknown — keep waiting.
		retry := 15 * time.Second
		return promotion.StepResult{
			Status:     kargoapi.PromotionStepStatusRunning,
			Message:    joinIssues(issues),
			Output:     map[string]any{"resources": details},
			RetryAfter: &retry,
		}, nil
	}
}

// criteria converts the step config into health checker input.
func (c Config) criteria() (*health.Criteria, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("could not encode health check criteria: %w", err)
	}
	var input health.Input
	if err := json.Unmarshal(b, &input); err != nil {
		return nil, fmt.Errorf("could not encode health check criteria: %w", err)
	}
	return &health.Criteria{Kind: CheckerKindFlux, Input: input}, nil
}

func errored() promotion.StepResult {
	return promotion.StepResult{Status: kargoapi.PromotionStepStatusErrored}
}

func joinIssues(issues []string) string {
	if len(issues) == 0 {
		return "waiting for Flux"
	}
	msg := issues[0]
	if len(issues) > 1 {
		msg = fmt.Sprintf("%s (and %d more)", msg, len(issues)-1)
	}
	return msg
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// Register wires Hecate's Flux support into Kargo's step and health checker
// registries. Call once from main() before starting the controller.
func Register(cl client.Client) {
	promotion.DefaultStepRunnerRegistry.MustRegister(
		promotion.StepRunnerRegistration{
			Name:  StepKindFluxWait,
			Value: newFluxWaiter,
		},
	)
	health.RegisterChecker(NewChecker(cl))
}
