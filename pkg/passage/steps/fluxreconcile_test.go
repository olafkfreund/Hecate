package steps

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// corev1 too: a clusterRef names a Secret, and a scheme that knows only the
	// Flux kinds cannot hold one. The same omission broke `hecate approve`
	// against an evidence-bound Gate, where it surfaced as "no kind is
	// registered for the type v1.Secret" and read like an RBAC problem.
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
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

// flux-reconcile could wait on a remote cluster but not nudge it: flux-wait and
// the health check both took clusterRef and this step did not. The asymmetry
// presented as remote environments simply being slower than local ones, for no
// reason visible anywhere in the Gate — they were moving at the remote Flux's
// own interval while the local one was being prompted.
//
// The object exists LOCALLY and the kubeconfig points somewhere unreachable, so
// succeeding would prove the step ignored clusterRef and nudged the local
// cluster. Failing to connect is the pass: it can only have looked elsewhere.
func TestFluxReconcileNudgesTheClusterItIsPointedAt(t *testing.T) {
	local := reconcileClient(t,
		fluxObject("Kustomization", "kustomize.toolkit.fluxcd.io/v1", "acme", "podinfo"),
		unreachableCluster("elsewhere"),
	)

	cfg := FluxReconcileConfig{
		Resources:  []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
		ClusterRef: &v1alpha1.LocalSecretRef{Name: "elsewhere"},
	}
	if _, err := NewFluxReconcile(local, false).Run(context.Background(), gitCtx(t, t.TempDir(), cfg)); err == nil {
		t.Fatal("the step succeeded against an unreachable cluster — it nudged the local one instead")
	}

	// And without clusterRef the same resource is nudged locally, so the
	// failure above is about where it looked and not about the resource.
	mustRun(t, NewFluxReconcile(local, false), gitCtx(t, t.TempDir(), FluxReconcileConfig{
		Resources: []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
	}))
}

// A cluster reference is not a way around the tenant rule. A step that can
// annotate any namespace's Kustomization can trigger any tenant's deployment,
// and that is as true on someone else's cluster as on this one. Validation runs
// before the cluster is resolved, so this refuses without going near a network.
func TestFluxReconcileRefusesAnotherNamespaceRemotely(t *testing.T) {
	local := reconcileClient(t, unreachableCluster("elsewhere"))

	_, err := NewFluxReconcile(local, false).Run(context.Background(), gitCtx(t, t.TempDir(), FluxReconcileConfig{
		Resources:  []health.FluxResource{{Kind: "Kustomization", Name: "theirs", Namespace: "other-team"}},
		ClusterRef: &v1alpha1.LocalSecretRef{Name: "elsewhere"},
	}))
	if err == nil {
		t.Fatal("a cross-namespace reference was accepted because it named another cluster")
	}
	if !strings.Contains(err.Error(), "cross-namespace") && !strings.Contains(err.Error(), "namespace") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// unreachableCluster is a kubeconfig Secret naming an address nothing answers
// on. Building a client from it succeeds — client.New does not dial — so the
// step gets a usable client that fails on first use, which is exactly the shape
// of a rotated credential or a cluster behind a broken route.
func unreachableCluster(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Data: map[string][]byte{"value": []byte(`apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster: {server: "https://127.0.0.1:1"}
contexts:
  - name: remote
    context: {cluster: remote, user: remote}
current-context: remote
users:
  - name: remote
    user: {token: abc123}
`)},
	}
}
