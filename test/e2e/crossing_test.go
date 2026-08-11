//go:build e2e

// Package e2e exercises Hecate against a real Kubernetes API server.
//
// What it proves that unit tests cannot: the generated CRDs actually accept the
// objects the controllers write. A CRD that has drifted from the Go types — a
// missing field, a bad enum, a rejected pattern — is invisible to a fake client
// and fatal in a cluster.
//
// The reconcilers run in-process against the cluster's API rather than as a
// deployed workload, so this needs no image and no chart. Once the Helm chart
// lands (#16) an in-cluster variant should follow; this covers the API contract
// either way.
//
// Run with: make e2e   (after: make cluster)
package e2e

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/beacon"
	"github.com/olafkfreund/hecate/pkg/gate"
)

const namespace = "hecate-e2e"

func newClient(t *testing.T) client.Client {
	t.Helper()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("no cluster reachable (run 'make cluster'): %v", err)
	}
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

// freshNamespace gives each run a clean slate and removes it afterwards, so a
// failed run never poisons the next one.
func freshNamespace(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}

	_ = c.Delete(ctx, ns)
	waitFor(t, 2*time.Minute, "old namespace to go away", func() bool {
		err := c.Get(ctx, types.NamespacedName{Name: namespace}, &corev1.Namespace{})
		return apierrors.IsNotFound(err)
	})
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
}

// localRegistry serves images from inside the test process. The Beacon resolver
// also runs in-process, so it can reach this — which keeps the test off the
// public internet and makes it deterministic.
func localRegistry(t *testing.T, tags ...string) (repo string, digests map[string]string) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repo = u.Host + "/e2e/app"
	digests = map[string]string{}

	for i, tag := range tags {
		img, err := random.Image(int64(64+i), 1)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := name.NewTag(repo + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		digests[tag] = d.String()
	}
	return repo, digests
}

// TestCrossingAgainstRealAPI drives a Bundle from discovery through two Gates,
// with every object round-tripping through a real API server.
func TestCrossingAgainstRealAPI(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	freshNamespace(t, c)

	repo, digests := localRegistry(t, "1.0.0", "1.1.0")

	beaconReconciler := &beacon.Reconciler{Client: c, Resolver: &beacon.Resolver{Client: c}}
	gateReconciler := &gate.Reconciler{Client: c}

	// --- discovery -------------------------------------------------------
	if err := c.Create(ctx, &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Watch:    []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{Repo: repo}}},
		},
	}); err != nil {
		t.Fatalf("the API server rejected a Beacon — the CRD has drifted from the Go types: %v", err)
	}

	reconcileBeacon(t, beaconReconciler, "app")

	var bundles v1alpha1.BundleList
	if err := c.List(ctx, &bundles, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(bundles.Items) != 1 {
		t.Fatalf("got %d Bundles, want 1", len(bundles.Items))
	}
	emitted := bundles.Items[0]
	if got := emitted.Spec.Artifacts[0].Image.Digest; got != digests["1.1.0"] {
		t.Errorf("Bundle pinned %s, want the digest of 1.1.0 (%s)", got, digests["1.1.0"])
	}

	// Discovery must be idempotent against a real API server too, where the
	// AlreadyExists path is a genuine server response rather than a fake.
	reconcileBeacon(t, beaconReconciler, "app")
	if err := c.List(ctx, &bundles, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(bundles.Items) != 1 {
		t.Fatalf("a second poll minted %d Bundles, want 1", len(bundles.Items))
	}

	// --- gates -----------------------------------------------------------
	steps := &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{Uses: "flux-wait"}}}

	mustCreate(t, c, &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Admits:  []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: steps,
			Auto:    true,
		},
	})
	mustCreate(t, c, &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Admits: []v1alpha1.Admission{{
				From:  v1alpha1.BundleOrigin{Beacon: "app"},
				After: []string{"staging"},
			}},
			Passage: steps,
			Auto:    true,
		},
	})

	// Production must refuse until staging has cleared the Bundle.
	reconcileGate(t, gateReconciler, "production")
	if n := countPassages(t, c, "production"); n != 0 {
		t.Fatalf("production started %d Passages before staging cleared, want 0", n)
	}

	reconcileGate(t, gateReconciler, "staging")
	stagingPassages := passagesFor(t, c, "staging")
	if len(stagingPassages) != 1 {
		t.Fatalf("staging started %d Passages, want 1", len(stagingPassages))
	}

	// Stand in for the Passage controller: flux-wait needs real Flux resources,
	// which belong to the in-cluster e2e once the chart lands.
	p := stagingPassages[0]
	p.Status.Phase = v1alpha1.PassageSucceeded
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatalf("the API server rejected a Passage status: %v", err)
	}
	reconcileGate(t, gateReconciler, "staging")

	var cleared v1alpha1.Bundle
	if err := c.Get(ctx, types.NamespacedName{Name: emitted.Name, Namespace: namespace}, &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.HasCleared("staging") {
		t.Fatal("a successful crossing was not recorded on the Bundle")
	}

	// Now production admits it.
	reconcileGate(t, gateReconciler, "production")
	if n := countPassages(t, c, "production"); n != 1 {
		t.Errorf("production started %d Passages after staging cleared, want 1", n)
	}
}

func reconcileBeacon(t *testing.T, r *beacon.Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	}); err != nil {
		t.Fatalf("reconciling Beacon %s: %v", name, err)
	}
}

func reconcileGate(t *testing.T, r *gate.Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	}); err != nil {
		t.Fatalf("reconciling Gate %s: %v", name, err)
	}
}

func mustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("the API server rejected %T %s — the CRD may have drifted: %v",
			obj, obj.GetName(), err)
	}
}

func passagesFor(t *testing.T, c client.Client, gateName string) []v1alpha1.Passage {
	t.Helper()
	var l v1alpha1.PassageList
	if err := c.List(context.Background(), &l,
		client.InNamespace(namespace), client.MatchingLabels{gate.LabelGate: gateName},
	); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func countPassages(t *testing.T, c client.Client, gateName string) int {
	t.Helper()
	return len(passagesFor(t, c, gateName))
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestMain(m *testing.M) {
	// Fail fast and legibly rather than with a wall of connection errors.
	if _, err := ctrl.GetConfig(); err != nil {
		println("e2e: no cluster reachable — run 'make cluster' first")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// jsonOf marshals a value into the apiextensions JSON wrapper the API uses for
// opaque step and check configuration.
func jsonOf(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}
