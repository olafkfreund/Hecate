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
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/beacon"
	"github.com/olafkfreund/hecate/pkg/gate"
)

// namespace is set per test by freshNamespace. A package-level var rather than
// a parameter threaded through twenty call sites; safe because these tests run
// sequentially, and they must — they share one cluster. Do not add t.Parallel.
var namespace string

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

// freshNamespace gives the test a namespace of its own.
//
// It used to delete and recreate one shared name, waiting only for the
// Namespace object to 404 before recreating it. Namespace teardown finalises
// asynchronously, so a Bundle created into the new namespace could be collected
// by the old one's GC — the Beacon reported a latestBundle that no longer
// existed, which is #110. A name nothing has used before cannot be caught by
// anything's teardown, and there is nothing to wait for.
func freshNamespace(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()

	namespace = uniqueNamespace(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace %s: %v", namespace, err)
	}
	// Best effort, and deliberately not waited on: the next test uses a
	// different name, so nothing depends on this finishing.
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
}

// uniqueNamespace names it after the test, so a failure says which test left
// the objects behind, with a suffix so a re-run never reuses a name that may
// still be finalising.
func uniqueNamespace(t *testing.T) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
	if len(name) > 40 {
		name = name[:40]
	}
	return fmt.Sprintf("%s-%d", strings.Trim(name, "-"), time.Now().UnixNano()%1_000_000)
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

	// This test drives the reconcilers itself. If Hecate is also deployed, both
	// write the same objects' status and collide — not a flake, just two
	// controllers for one resource. The in-cluster test covers this ground once
	// Hecate is installed; run this one before installing.
	skipIfHecateInstalled(t, c)

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

	// Read by the name the Beacon reports rather than by listing. On k3s the
	// two disagree: a GET finds a Bundle that a LIST — namespaced *and*
	// cluster-wide, at a newer resourceVersion, still empty five seconds later
	// — does not return. That is a storage-layer inconsistency under kine, not
	// something Hecate can fix, and it cost #110 four wrong diagnoses. A GET is
	// also simply the better assertion: it checks the object the system named,
	// instead of counting rows.
	emitted := emittedBundle(t, c, "the Beacon to emit a Bundle")
	if got := emitted.Spec.Artifacts[0].Image.Digest; got != digests["1.1.0"] {
		t.Errorf("Bundle pinned %s, want the digest of 1.1.0 (%s)", got, digests["1.1.0"])
	}

	// Discovery must be idempotent against a real API server too, where the
	// AlreadyExists path is a genuine server response rather than a fake.
	//
	// Asserted on the *name*, which is what idempotency means here: the name is
	// derived from the artifact digest, so a second Bundle could only appear
	// under a different one — and `latestBundle` would then have moved.
	// Counting objects would test the same thing less precisely, through the
	// read that does not work.
	reconcileBeacon(t, beaconReconciler, "app")
	if again := emittedBundle(t, c, "a second poll to mint no second Bundle"); again.Name != emitted.Name {
		t.Errorf("a second poll emitted %s, having already emitted %s", again.Name, emitted.Name)
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

// emittedBundle waits for the Beacon to name a Bundle in its status and returns
// that Bundle, read by name.
//
// **Deliberately not a list.** On k3s a GET finds Bundles that a LIST does not
// return — namespaced and cluster-wide, at a newer resourceVersion, still
// missing five seconds later. That is a storage inconsistency in kine rather
// than anything Hecate does, and #110 spent four rounds mistaking it for a bug
// in the emit path because every assertion here was built on counting rows.
//
// Reading by name is not a workaround for it either. The Beacon's contract is
// "emit a Bundle and say which one"; checking the object it named tests that
// contract directly, where a count only tests it by inference.
func emittedBundle(t *testing.T, c client.Client, why string) *v1alpha1.Bundle {
	t.Helper()
	ctx := context.Background()

	var bundle v1alpha1.Bundle
	waitFor(t, 60*time.Second, why, func() bool {
		var b v1alpha1.Beacon
		if err := c.Get(ctx, types.NamespacedName{Name: "app", Namespace: namespace}, &b); err != nil {
			return false
		}
		if b.Status.LatestBundle == "" {
			return false
		}
		return c.Get(ctx, types.NamespacedName{
			Name: b.Status.LatestBundle, Namespace: namespace,
		}, &bundle) == nil
	}, func() string { return beaconStatus(t, c) }, func() string { return bundleForensics(t, c) })

	// Still worth knowing when the two reads disagree, because the Gate
	// controller lists Bundles the same way the test used to. Logged rather
	// than failed: it is the cluster's inconsistency, not ours, and hiding it
	// entirely would leave nobody any the wiser next time.
	var listed v1alpha1.BundleList
	if err := c.List(ctx, &listed, client.InNamespace(namespace)); err == nil && len(listed.Items) == 0 {
		t.Logf("note: a LIST returns no Bundles while a GET finds %s — the k3s/kine "+
			"inconsistency from #110. Harmless here; the Gate would see it a "+
			"reconcile later.", bundle.Name)
	}
	return &bundle
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

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool, diagnose ...func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Second)
	}
	// A timeout with no context is the least useful failure a test can produce,
	// and in CI it is often the only thing you get to see.
	msg := fmt.Sprintf("timed out after %s waiting for %s", timeout, what)
	for _, d := range diagnose {
		msg += "\n  " + d()
	}
	t.Fatal(msg)
}

// The Fides environment reference is validated by the API server, not by
// Hecate: the CRD carries a UUID pattern, so a typo is refused when the Gate is
// applied rather than discovered by a compliance check quietly reading the
// wrong environment's policy. That enforcement only exists against a real API
// server, which is why this test lives here.
func TestFidesEnvironmentIsValidatedAtAdmission(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	freshNamespace(t, c)

	gate := func(name, environment string) *v1alpha1.Gate {
		return &v1alpha1.Gate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: v1alpha1.GateSpec{
				Admits:   []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
				Evidence: &v1alpha1.EvidenceConfig{FidesEnvironment: environment},
			},
		}
	}

	for _, environment := range []string{"production", "", "7f3a1c2e-0000-4000-8000", "not-a-uuid-at-all"} {
		if err := c.Create(ctx, gate("rejected", environment)); err == nil {
			t.Errorf("the API server accepted %q as a Fides environment", environment)
			_ = c.Delete(ctx, gate("rejected", environment))
		}
	}

	if err := c.Create(ctx, gate("accepted", "7f3a1c2e-0000-4000-8000-000000009b04")); err != nil {
		t.Errorf("a real environment UUID was rejected: %v", err)
	}
}

func TestMain(m *testing.M) {
	// Fail fast and legibly rather than with a wall of connection errors.
	if _, err := ctrl.GetConfig(); err != nil {
		println("e2e: no cluster reachable — run 'make cluster' first")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// beaconStatus renders why a Beacon has not emitted, for failure messages. A
// Beacon that cannot resolve a source records the reason in its Ready condition
// rather than failing the reconcile, so without this the test reports the
// symptom and hides the cause.
func beaconStatus(t *testing.T, c client.Client) string {
	t.Helper()
	var b v1alpha1.Beacon
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "app", Namespace: namespace}, &b); err != nil {
		return "Beacon unreadable: " + err.Error()
	}
	for _, cond := range b.Status.Conditions {
		if cond.Type == v1alpha1.ConditionReady {
			return "Beacon Ready=" + string(cond.Status) + " " + cond.Reason + ": " + cond.Message
		}
	}
	return "Beacon has no Ready condition yet"
}

// everyBundle lists Bundles across all namespaces. If the Beacon recorded a
// latestBundle but the namespaced List sees nothing, the object either went
// somewhere unexpected or was removed after creation — and which one matters.
// bundleForensics answers the question the Bundle listing cannot: was the
// Bundle never created, or created and then removed?
//
// #110 recurred after a fix based on the listing alone, which was not enough
// evidence to diagnose from. The Beacon reports a latestBundle that no listing
// finds, and these two facts separate the cases:
//
//   - a GET by that exact name is the API server's own answer, not a list that
//     might lag;
//   - the controller emits BundleEmitted only when its Create actually created
//     something, so no event means Create returned AlreadyExists for an object
//     that does not exist — a very different bug from one that deletes it.
func bundleForensics(t *testing.T, c client.Client) string {
	t.Helper()
	ctx := context.Background()

	var beacon v1alpha1.Beacon
	if err := c.Get(ctx, types.NamespacedName{Name: "app", Namespace: namespace}, &beacon); err != nil {
		return "the Beacon itself is unreadable: " + err.Error()
	}
	name := beacon.Status.LatestBundle
	if name == "" {
		return "the Beacon reports no latestBundle, so it never got as far as emitting"
	}

	out := fmt.Sprintf("namespace %s, latestBundle %s\n", namespace, name)

	var bundle v1alpha1.Bundle
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &bundle)
	switch {
	case err == nil:
		out += fmt.Sprintf("  GET by name: found it, created %s, deletionTimestamp %v, rv %s\n",
			bundle.CreationTimestamp, bundle.DeletionTimestamp, bundle.ResourceVersion)

		// The heart of #110: a GET by name finds a Bundle that a LIST does not
		// return, from the same uncached client, in the same namespace. Both
		// should be quorum reads, so this should be impossible.
		//
		// Comparing resourceVersions says which story to believe. A list whose
		// own rv is older than the object's is a stale read — k3d runs k3s,
		// which is backed by kine rather than etcd, and that is where a
		// list/get disagreement would come from. An rv at or past the object's
		// means the list was current and genuinely did not contain it, which
		// would be a much stranger bug and worth knowing early.
		var again v1alpha1.BundleList
		if err := c.List(ctx, &again, client.InNamespace(namespace)); err != nil {
			out += fmt.Sprintf("  re-LIST: %s\n", err)
		} else {
			out += fmt.Sprintf("  re-LIST now: %d item(s), list rv %s\n",
				len(again.Items), again.ResourceVersion)
		}

		// The remaining question, once the resourceVersions have shown the list
		// is not merely behind: does the object ever turn up, or has the watch
		// cache lost it for good? The first is a lag the Gate's own informer
		// would also recover from; the second would mean a Bundle could stay
		// invisible to the Gate indefinitely, which is a product problem rather
		// than a test one.
		time.Sleep(5 * time.Second)
		var later v1alpha1.BundleList
		if err := c.List(ctx, &later, client.InNamespace(namespace)); err != nil {
			out += fmt.Sprintf("  re-LIST after 5s: %s\n", err)
		} else {
			out += fmt.Sprintf("  re-LIST after 5s: %d item(s), list rv %s\n",
				len(later.Items), later.ResourceVersion)
		}
	default:
		out += fmt.Sprintf("  GET by name: %s\n", err)
	}

	var events corev1.EventList
	if err := c.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		out += "  events unlistable: " + err.Error()
		return out
	}
	if len(events.Items) == 0 {
		out += "  no events at all in the namespace"
		return out
	}
	out += "  events:"
	for i := range events.Items {
		e := &events.Items[i]
		out += fmt.Sprintf("\n    %s %s/%s: %s", e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
	}
	return out
}

func everyBundle(t *testing.T, c client.Client) string {
	t.Helper()
	var all v1alpha1.BundleList
	if err := c.List(context.Background(), &all); err != nil {
		return "Bundles unlistable: " + err.Error()
	}
	if len(all.Items) == 0 {
		return "no Bundles exist in any namespace"
	}
	out := "Bundles cluster-wide:"
	for _, b := range all.Items {
		out += fmt.Sprintf(" %s/%s", b.Namespace, b.Name)
	}
	return out
}

// skipIfHecateInstalled skips when a deployed controller would race this test.
func skipIfHecateInstalled(t *testing.T, c client.Client) {
	t.Helper()
	var deployments unstructured.UnstructuredList
	deployments.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apps", Version: "v1", Kind: "DeploymentList",
	})
	if err := c.List(context.Background(), &deployments,
		client.InNamespace("hecate-system"),
		client.MatchingLabels{"app.kubernetes.io/name": "hecate"},
	); err == nil && len(deployments.Items) > 0 {
		t.Skip("Hecate is deployed in-cluster; it would race this test's own reconcilers")
	}
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
