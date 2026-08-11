package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepFluxReconcile is the value used in `steps[].uses`.
const StepFluxReconcile = "flux-reconcile"

// ReasonFluxPatchFailed means the annotation could not be written. A rejected
// patch is usually RBAC, which retrying will not fix, so it is reported
// separately from a resource that is merely absent.
const ReasonFluxPatchFailed = "FluxPatchFailed"

// reconcileAnnotation is Flux's own trigger. Flux acts when the value changes;
// it never reads it.
const reconcileAnnotation = "reconcile.fluxcd.io/requestedAt"

// FluxReconcileConfig is the `with:` block of a flux-reconcile step.
type FluxReconcileConfig struct {
	// Resources are the Flux objects to nudge — usually the GitRepository the
	// commit went to, and sometimes the Kustomization that consumes it.
	Resources []health.FluxResource `json:"resources"`
}

// The only write Hecate performs against Flux, and the narrowest one that can
// trigger a sync. Detached from the type on purpose: rbac markers are
// package-level, and one attached to a declaration is silently ignored.
//
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;helmcharts;helmrepositories;buckets,verbs=patch

// FluxReconcile asks Flux to sync now rather than at its next interval.
//
// The annotation is a doorbell, not a desired state: it changes nothing about
// what Flux will do, only when. Git is still the rendezvous — the commit is
// already pushed and Flux would find it on its own. Without this a promotion's
// visible latency is the source interval, which is minutes by default and an
// hour in plenty of real fleets.
type FluxReconcile struct {
	client              client.Client
	allowCrossNamespace bool
}

// NewFluxReconcile returns a flux-reconcile step.
func NewFluxReconcile(c client.Client, allowCrossNamespace bool) *FluxReconcile {
	return &FluxReconcile{client: c, allowCrossNamespace: allowCrossNamespace}
}

// Name implements passage.Runner.
func (f *FluxReconcile) Name() string { return StepFluxReconcile }

// Run implements passage.Runner.
func (f *FluxReconcile) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[FluxReconcileConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepFluxReconcile, err)
	}
	// Validated by the same rules as flux-wait, including the cross-namespace
	// refusal: a step that can annotate any namespace's Kustomization is a step
	// that can trigger any tenant's deployment.
	if err := (health.FluxConfig{Resources: cfg.Resources}).Validate(sc.Namespace, f.allowCrossNamespace); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepFluxReconcile, err)
	}

	// Stamped from the Passage, not the clock, so re-running a crossing does
	// not ring the doorbell again — the value is unchanged and Flux ignores it.
	// One nudge per attempt is the intent; flux-wait does the waiting.
	stamp := sc.StartedAt.UTC().Format(time.RFC3339Nano)
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{reconcileAnnotation: stamp}},
	})
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonFluxPatchFailed, "%s: %s", StepFluxReconcile, err)
	}

	for i, ref := range cfg.Resources {
		gvk, err := ref.GVK()
		if err != nil {
			return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
				"%s: resources[%d]: %s", StepFluxReconcile, i, err)
		}
		ns := ref.Namespace
		if ns == "" {
			ns = sc.Namespace
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetNamespace(ns)
		obj.SetName(ref.Name)

		// A merge patch rather than read-modify-write: there is nothing to read,
		// and nothing here can conflict with Flux's own writes to the object.
		err = f.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
		switch {
		case apierrors.IsNotFound(err):
			return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
				"%s: no %s named %s in %s — check the name, and that Flux owns it",
				StepFluxReconcile, gvk.Kind, ref.Name, ns)
		case apierrors.IsForbidden(err):
			return passage.StepResult{}, passage.FailTerminal(ReasonFluxPatchFailed,
				"%s: not allowed to annotate %s %s/%s — Hecate needs patch on %s",
				StepFluxReconcile, gvk.Kind, ns, ref.Name, gvk.GroupVersion().Group)
		case err != nil:
			return passage.StepResult{}, passage.Fail(ReasonFluxPatchFailed,
				"%s: annotating %s %s/%s: %s", StepFluxReconcile, gvk.Kind, ns, ref.Name, err)
		}
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("asked Flux to reconcile %s now", plural(len(cfg.Resources), "resource")),
		Output:  map[string]any{"requestedAt": stamp, "resources": len(cfg.Resources)},
	}, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
