package steps

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

func fluxObject(kind, apiVersion, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func reconcileClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
		{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := &unstructured.UnstructuredList{}
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), list)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func annotation(t *testing.T, c client.Client, kind, apiVersion, ns, name string) string {
	t.Helper()
	obj := fluxObject(kind, apiVersion, ns, name)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return obj.GetAnnotations()[reconcileAnnotation]
}

func TestFluxReconcileRingsTheDoorbell(t *testing.T) {
	c := reconcileClient(t,
		fluxObject("GitRepository", "source.toolkit.fluxcd.io/v1", "acme", "fleet"),
		fluxObject("Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo"),
	)

	res := mustRun(t, NewFluxReconcile(c, false), gitCtx(t, t.TempDir(), FluxReconcileConfig{
		Resources: []health.FluxResource{
			{Kind: "GitRepository", Name: "fleet"},
			{Kind: "Kustomization", Name: "podinfo"},
		},
	}))

	stamp := passageStart.UTC().Format("2006-01-02T15:04:05")
	for _, o := range [][3]string{
		{"GitRepository", "source.toolkit.fluxcd.io/v1", "fleet"},
		{"Kustomization", "kustomize.toolkit.fluxcd.io/v1", "podinfo"},
	} {
		got := annotation(t, c, o[0], o[1], "acme", o[2])
		if !strings.HasPrefix(got, stamp) {
			t.Errorf("%s annotation = %q, want it stamped from the Passage start", o[0], got)
		}
	}
	if res.Output["resources"] != 2 {
		t.Errorf("output resources = %v", res.Output["resources"])
	}
}

// Stamping from the Passage rather than the clock means a requeue does not ring
// the doorbell twice. Flux acts on a change of value, so an unchanged one is
// exactly the no-op a retry should be.
func TestFluxReconcileIsIdempotentWithinAPassage(t *testing.T) {
	c := reconcileClient(t, fluxObject("Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo"))
	cfg := FluxReconcileConfig{Resources: []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}}}
	step := NewFluxReconcile(c, false)

	mustRun(t, step, gitCtx(t, t.TempDir(), cfg))
	first := annotation(t, c, "Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo")
	mustRun(t, step, gitCtx(t, t.TempDir(), cfg))
	second := annotation(t, c, "Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo")

	if first != second {
		t.Errorf("a re-run changed the annotation:\n  %s\n  %s", first, second)
	}
}

func TestFluxReconcileRefusals(t *testing.T) {
	c := reconcileClient(t, fluxObject("Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo"))

	for _, tc := range []struct {
		name   string
		cfg    FluxReconcileConfig
		reason string
		says   string
	}{
		{"no resources", FluxReconcileConfig{}, ReasonInvalidConfig, "required"},
		{
			"a resource that is not there",
			FluxReconcileConfig{Resources: []health.FluxResource{{Kind: "Kustomization", Name: "ghost"}}},
			ReasonInvalidConfig, "no Kustomization named ghost",
		},
		{
			"an unknown kind",
			FluxReconcileConfig{Resources: []health.FluxResource{{Kind: "Sprocket", Name: "x"}}},
			ReasonInvalidConfig, "apiVersion",
		},
		{
			// A step that can annotate any namespace's Kustomization can trigger
			// any tenant's deployment.
			"another namespace",
			FluxReconcileConfig{Resources: []health.FluxResource{
				{Kind: "Kustomization", Name: "podinfo", Namespace: "other"},
			}},
			ReasonInvalidConfig, "namespace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFluxReconcile(c, false).Run(context.Background(), gitCtx(t, t.TempDir(), tc.cfg))
			if !passage.IsTerminal(err) {
				t.Fatalf("err = %v, want a terminal failure", err)
			}
			if passage.ReasonOf(err) != tc.reason {
				t.Errorf("reason = %s, want %s", passage.ReasonOf(err), tc.reason)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q: %v", tc.says, err)
			}
		})
	}
}

// The step must not be the thing that decides a promotion failed: Flux would
// have found the commit at its next interval anyway.
func TestFluxReconcileSucceedsBeforeTheWait(t *testing.T) {
	c := reconcileClient(t, fluxObject("Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo"))
	res := mustRun(t, NewFluxReconcile(c, false), gitCtx(t, t.TempDir(), FluxReconcileConfig{
		Resources: []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
	}))
	if res.Phase != v1alpha1.StepSucceeded || res.RetryAfter != 0 {
		t.Errorf("phase = %s, retryAfter = %s — nudging is not waiting", res.Phase, res.RetryAfter)
	}
}
