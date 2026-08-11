package health

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/flux"
)

// CheckerFlux is the name used in `watch[].uses`.
const CheckerFlux = "flux"

// fluxAPIVersions maps the Flux kinds we understand to their current API
// version, so users need only write `kind: Kustomization`. An explicit
// apiVersion always wins — the escape hatch for kinds we have not enumerated
// and for future API bumps.
var fluxAPIVersions = map[string]string{
	"Kustomization":  "kustomize.toolkit.fluxcd.io/v1",
	"HelmRelease":    "helm.toolkit.fluxcd.io/v2",
	"GitRepository":  "source.toolkit.fluxcd.io/v1",
	"OCIRepository":  "source.toolkit.fluxcd.io/v1",
	"HelmChart":      "source.toolkit.fluxcd.io/v1",
	"HelmRepository": "source.toolkit.fluxcd.io/v1",
	"Bucket":         "source.toolkit.fluxcd.io/v1",
}

// FluxConfig is the `with:` block of a Flux health check. The same shape is
// accepted by the flux-wait step, so a Gate can wait on exactly what it then
// continuously watches.
type FluxConfig struct {
	// Resources are the Flux objects to assess.
	Resources []FluxResource `json:"resources"`
	// ExpectedRevision, when set, requires Flux to have applied this revision.
	// Usually wired from an earlier step: ${{ steps.commit.sha }}.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	// FailAfter is a Go duration bounding how long a resource may be un-Ready
	// before it is reported Degraded rather than Progressing. Default 10m.
	FailAfter string `json:"failAfter,omitempty"`
}

// FluxResource identifies one Flux object.
type FluxResource struct {
	Kind string `json:"kind"`
	// APIVersion overrides the default for Kind.
	APIVersion string `json:"apiVersion,omitempty"`
	Name       string `json:"name"`
	// Namespace defaults to the Gate's namespace.
	Namespace string `json:"namespace,omitempty"`
}

func (r FluxResource) gvk() (schema.GroupVersionKind, error) {
	av := r.APIVersion
	if av == "" {
		var ok bool
		if av, ok = fluxAPIVersions[r.Kind]; !ok {
			return schema.GroupVersionKind{}, fmt.Errorf(
				"no default apiVersion for kind %q; set apiVersion explicitly", r.Kind)
		}
	}
	gv, err := schema.ParseGroupVersion(av)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid apiVersion %q: %w", av, err)
	}
	return gv.WithKind(r.Kind), nil
}

// Validate checks the config is usable before anything is queried.
//
// gateNamespace is the namespace of the Gate this config belongs to.
// allowCrossNamespace relaxes the tenant boundary; see D11.
func (c FluxConfig) Validate(gateNamespace string, allowCrossNamespace bool) error {
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
		if !allowCrossNamespace && r.Namespace != "" && r.Namespace != gateNamespace {
			// Refused, not defaulted-away: silently rewriting the namespace
			// would watch something the author did not ask for.
			return fmt.Errorf(
				"resources[%d]: namespace %q is not %q — cross-namespace references are "+
					"refused by default, matching Flux's own --no-cross-namespace-refs. "+
					"Start the controller with --no-cross-namespace-refs=false to allow them",
				i, r.Namespace, gateNamespace)
		}
	}
	_, err := c.failAfter()
	return err
}

func (c FluxConfig) failAfter() (time.Duration, error) {
	if c.FailAfter == "" {
		return flux.DefaultFailAfter, nil
	}
	d, err := time.ParseDuration(c.FailAfter)
	if err != nil {
		return 0, fmt.Errorf("invalid failAfter %q: %w", c.FailAfter, err)
	}
	return d, nil
}

// Hecate reads Flux resources and never writes them — see D3. The one future
// exception is the reconcile annotation in flux-reconcile (#20), which will
// need `patch` on Kustomizations and HelmReleases, scoped narrowly and no wider.
//
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;helmcharts;helmrepositories;buckets,verbs=get;list;watch

// FluxChecker assesses Flux resources.
type FluxChecker struct {
	client client.Client
	// AllowCrossNamespace permits a Gate to watch resources outside its own
	// namespace. False by default, matching the posture Flux ships on five of
	// its own controllers and asks integrating controllers to adopt.
	//
	// Left open, one team's Gate can watch another team's Kustomization — a
	// tenant-isolation hole, and a channel for inferring what other teams run.
	AllowCrossNamespace bool
}

// NewFluxChecker returns a Checker backed by the given cluster client, refusing
// cross-namespace references.
func NewFluxChecker(c client.Client) *FluxChecker { return &FluxChecker{client: c} }

// AllowingCrossNamespace returns a checker that permits references outside the
// Gate's namespace. For single-tenant clusters that genuinely want it.
func (f *FluxChecker) AllowingCrossNamespace(allow bool) *FluxChecker {
	f.AllowCrossNamespace = allow
	return f
}

// Name implements Checker.
func (f *FluxChecker) Name() string { return CheckerFlux }

// Check implements Checker.
func (f *FluxChecker) Check(ctx context.Context, req Request) v1alpha1.HealthReport {
	cfg, err := DecodeConfig[FluxConfig](req)
	if err != nil {
		return Unknown("flux health check: %s", err)
	}
	if err := cfg.Validate(req.Namespace, f.AllowCrossNamespace); err != nil {
		return Unknown("flux health check: %s", err)
	}

	status, issues, details := f.Evaluate(ctx, cfg, req.Namespace)

	report := v1alpha1.HealthReport{Status: status, Issues: issues}
	if encoded, err := json.Marshal(details); err == nil {
		report.Details = &apiextensionsv1.JSON{Raw: encoded}
	}
	return report
}

// Evaluate resolves every configured resource and reduces them to one status.
// Exported so the flux-wait step can reuse it without going through the
// Checker interface.
func (f *FluxChecker) Evaluate(
	ctx context.Context, cfg FluxConfig, defaultNamespace string,
) (v1alpha1.Health, []string, map[string]any) {
	failAfter, _ := cfg.failAfter() // Validate already accepted it

	overall := v1alpha1.HealthHealthy
	var issues []string
	details := make(map[string]any, len(cfg.Resources))

	for _, ref := range cfg.Resources {
		ns := ref.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		gvk, _ := ref.gvk() // Validate already accepted it

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)

		var res flux.Result
		if err := f.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
			// A resource we cannot read is reported, never skipped. Silently
			// ignoring a missing Kustomization would make the Gate look
			// healthier than it is.
			res = flux.Result{
				Health: v1alpha1.HealthUnknown,
				Issues: []string{fmt.Sprintf("cannot read %s %s/%s: %s", ref.Kind, ns, ref.Name, err)},
			}
		} else {
			res = flux.Evaluate(obj, flux.Options{
				ExpectedRevision: cfg.ExpectedRevision,
				FailAfter:        failAfter,
			})
		}

		overall = overall.Merge(res.Health)
		issues = append(issues, res.Issues...)
		details[fmt.Sprintf("%s/%s/%s", ref.Kind, ns, ref.Name)] = map[string]any{
			"health":   string(res.Health),
			"revision": res.Revision,
		}
	}

	return overall, issues, details
}
