package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/flux"
	"github.com/olafkfreund/hecate/pkg/health"
)

// reconcileAnnotation is what Flux watches for a "do it now" request. Same key
// the flux-reconcile step uses, and the same one `flux reconcile` sets.
const reconcileAnnotation = "reconcile.fluxcd.io/requestedAt"

// FluxResource is one Flux object a Gate watches, and what is true of it.
type FluxResource struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Suspended means Flux is not reconciling it.
	//
	// The reason this whole screen exists. A suspended resource is cluster
	// state that git will not restore, so it outlives the debugging session it
	// was created for — and every crossing afterwards appears to succeed while
	// changing nothing, because the step that would have applied it is not
	// running. It is the one Flux state that fails silently.
	Suspended bool `json:"suspended"`
	// Health is what pkg/flux makes of its conditions.
	Health v1alpha1.Health `json:"health"`
	// Detail is the Ready condition's message, or why it is not Ready.
	Detail string `json:"detail,omitempty"`
	// Revision is what Flux has actually applied.
	Revision string `json:"revision,omitempty"`
	// LastHandled is the reconcile request Flux has acted on, so a caller can
	// tell its own "reconcile now" landed rather than watching for any change.
	LastHandled string `json:"lastHandled,omitempty"`
	// Missing means the Gate names it and the cluster does not have it. Not an
	// error: a Gate committed before its Kustomization exists is ordinary, and
	// reporting it as absent beats reporting it as unhealthy.
	Missing bool `json:"missing"`
}

// FluxResources is what a Gate watches, and the state of each.
//
// Read from the Gate's own health checks rather than from a list of everything
// Flux owns in the namespace: the question this answers is "what does this Gate
// depend on", and a screen listing every Kustomization in the namespace would
// invite someone to suspend one this Gate has nothing to do with.
func (o *Ops) FluxResources(ctx context.Context, namespace, gateName string) ([]FluxResource, error) {
	g, err := o.Gate(ctx, namespace, gateName)
	if err != nil {
		return nil, err
	}

	out := []FluxResource{}
	seen := map[string]bool{}

	for _, check := range g.Spec.Watch {
		cfg, err := fluxConfigOf(check)
		if err != nil || cfg == nil {
			// A check that is not a Flux check, or one whose config does not
			// parse. Neither is this function's business to report: the health
			// pipeline already surfaces a broken check as a Gate problem, and
			// failing the whole panel because one check is malformed would hide
			// the resources that are fine.
			continue
		}
		// A remote cluster needs its kubeconfig, and this panel deliberately
		// does not reach for one: suspending a resource on a cluster whose
		// credentials might be stale is the operation most likely to leave
		// someone unable to un-suspend it.
		if cfg.ClusterRef != nil {
			continue
		}
		for _, ref := range cfg.Resources {
			ns := ref.Namespace
			if ns == "" {
				ns = namespace
			}
			key := ref.Kind + "/" + ns + "/" + ref.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, o.readFlux(ctx, ref, ns))
		}
	}

	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// readFlux fetches one resource and reduces it to what the screen shows.
func (o *Ops) readFlux(ctx context.Context, ref health.FluxResource, namespace string) FluxResource {
	r := FluxResource{Kind: ref.Kind, Name: ref.Name, Namespace: namespace, Health: v1alpha1.HealthUnknown}

	gvk, err := ref.GVK()
	if err != nil {
		r.Detail = err.Error()
		return r
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			r.Missing = true
			r.Detail = "not in the cluster"
			return r
		}
		r.Detail = err.Error()
		return r
	}

	r.Suspended, _, _ = unstructured.NestedBool(obj.Object, "spec", "suspend")
	r.LastHandled, _, _ = unstructured.NestedString(obj.Object, "status", "lastHandledReconcileAt")

	res := flux.Evaluate(obj, flux.Options{})
	r.Health = res.Health
	r.Revision = res.Revision
	if len(res.Issues) > 0 {
		r.Detail = strings.Join(res.Issues, " · ")
	}
	return r
}

// SetFluxSuspend suspends or resumes one Flux resource a Gate watches.
//
// Scoped to the Gate's own resources rather than taking a free-form reference:
// the API's authorisation is per namespace, and a handler that suspended
// anything it was named would let a caller who may write in one namespace
// stop reconciliation in another simply by asking for it.
func (o *Ops) SetFluxSuspend(
	ctx context.Context, namespace, gateName, kind, name string, suspend bool,
) error {
	ref, err := o.resourceOfGate(ctx, namespace, gateName, kind, name)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
	if err != nil {
		return err
	}
	return o.patchFlux(ctx, ref, namespace, patch, "suspend")
}

// ReconcileFlux asks Flux to look at one resource now.
//
// Returns the stamp it wrote, so a caller can match it against
// status.lastHandledReconcileAt and know its own request was the one that
// landed rather than watching for any change and hoping.
func (o *Ops) ReconcileFlux(ctx context.Context, namespace, gateName, kind, name string) (string, error) {
	ref, err := o.resourceOfGate(ctx, namespace, gateName, kind, name)
	if err != nil {
		return "", err
	}
	// The clock, unlike the flux-reconcile step which stamps from the Passage:
	// there a re-run must not ring the doorbell twice, here a person pressing
	// the button twice means it twice.
	stamp := o.now().UTC().Format(time.RFC3339Nano)
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{reconcileAnnotation: stamp}},
	})
	if err != nil {
		return "", err
	}
	if err := o.patchFlux(ctx, ref, namespace, patch, "annotate"); err != nil {
		return "", err
	}
	return stamp, nil
}

// resourceOfGate finds a resource among the ones a Gate watches, refusing
// anything it does not.
func (o *Ops) resourceOfGate(
	ctx context.Context, namespace, gateName, kind, name string,
) (health.FluxResource, error) {
	g, err := o.Gate(ctx, namespace, gateName)
	if err != nil {
		return health.FluxResource{}, err
	}
	for _, check := range g.Spec.Watch {
		cfg, err := fluxConfigOf(check)
		if err != nil || cfg == nil || cfg.ClusterRef != nil {
			continue
		}
		for _, ref := range cfg.Resources {
			ns := ref.Namespace
			if ns == "" {
				ns = namespace
			}
			if ref.Kind == kind && ref.Name == name && ns == namespace {
				return ref, nil
			}
		}
	}
	return health.FluxResource{}, fmt.Errorf(
		"%s %q is not watched by Gate %s — this only operates on what the Gate depends on",
		kind, name, gateName)
}

func (o *Ops) patchFlux(
	ctx context.Context, ref health.FluxResource, namespace string, patch []byte, verb string,
) error {
	gvk, err := ref.GVK()
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(ref.Name)

	err = o.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("no %s named %s in %s — check the name, and that Flux owns it",
			gvk.Kind, ref.Name, namespace)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("not allowed to %s %s %s/%s — Hecate needs patch on %s",
			verb, gvk.Kind, namespace, ref.Name, gvk.GroupVersion().Group)
	}
	return err
}

// fluxConfigOf pulls the Flux config out of a health check, or nil when the
// check is not a Flux one.
func fluxConfigOf(check v1alpha1.HealthCheck) (*health.FluxConfig, error) {
	if check.Uses != health.CheckerFlux || check.With == nil || len(check.With.Raw) == 0 {
		return nil, nil
	}
	var cfg health.FluxConfig
	if err := json.Unmarshal(check.With.Raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
