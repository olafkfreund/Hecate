//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// The registry address depends on who is asking, which is not obvious and cost
// an hour the first time:
//
//   - containerd pulls images through k3d's mirror on :5001
//   - Flux's source-controller makes its own HTTP request and must use the
//     container's real port, :5000
const (
	registryFromHost    = "localhost:5001"
	registryFromCluster = "hecate-registry:5000"
)

var (
	ociRepoGVK = schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository",
	}
	kustomizationGVK = schema.GroupVersionKind{
		Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
	}
)

// publishImages pushes tagged images into the cluster's registry, so the
// deployed Beacon has something to discover without reaching the internet.
//
// Two addresses for one registry, which is not obvious: the test pushes from the
// host on :5001, and the Beacon in the cluster reads it as
// hecate-registry:5000. See the constants above.
func publishImages(t *testing.T, repo string, tags ...string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for i, tag := range tags {
		img, err := random.Image(int64(64+i), 1)
		if err != nil {
			t.Fatal(err)
		}
		// Insecure because the k3d registry is plain HTTP — the same reason the
		// Beacon needs spec.watch[].image.insecure to read it.
		ref, err := name.NewTag(registryFromHost+"/"+repo+":"+tag, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatalf("pushing %s: %v", ref, err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests[tag] = d.String()
	}
	return digests
}

// requireHecateInstalled skips unless the controller is actually deployed. The
// out-of-cluster test in crossing_test.go covers the API contract without it;
// this one is about the deployed system.
func requireHecateInstalled(t *testing.T, c client.Client) {
	t.Helper()
	var deployments unstructured.UnstructuredList
	deployments.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apps", Version: "v1", Kind: "DeploymentList",
	})
	if err := c.List(context.Background(), &deployments,
		client.InNamespace("hecate-system"),
		client.MatchingLabels{"app.kubernetes.io/name": "hecate"},
	); err != nil || len(deployments.Items) == 0 {
		t.Skip("Hecate is not installed in-cluster — run 'make install'")
	}
	if _, err := exec.LookPath("flux"); err != nil {
		t.Skip("the flux CLI is not on PATH")
	}
}

// publishArtifact pushes a Flux OCI artifact to the cluster-local registry, so
// the test needs no internet and no external registry account.
func publishArtifact(t *testing.T, tag string) {
	t.Helper()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hecate-e2e-payload\ndata:\n  tag: \"" + tag + "\"\n"
	if err := os.WriteFile(filepath.Join(manifests, "cm.yaml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("flux", "push", "artifact",
		"oci://"+registryFromHost+"/e2e-manifests:"+tag,
		"--path", manifests, "--source", "e2e", "--revision", tag,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("pushing OCI artifact: %v\n%s", err, out)
	}
}

func applyFluxSource(t *testing.T, c client.Client, tag string) {
	t.Helper()
	ctx := context.Background()

	repo := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval": "30s",
			"url":      "oci://" + registryFromCluster + "/e2e-manifests",
			"ref":      map[string]any{"tag": tag},
			"insecure": true, // plain HTTP: a local registry with no certificate
		},
	}}
	repo.SetGroupVersionKind(ociRepoGVK)
	repo.SetName("e2e-manifests")
	repo.SetNamespace(namespace)
	if err := c.Create(ctx, repo); err != nil {
		t.Fatalf("creating OCIRepository: %v", err)
	}

	ks := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"interval":        "30s",
			"prune":           true,
			"targetNamespace": namespace,
			"path":            "./",
			"sourceRef":       map[string]any{"kind": "OCIRepository", "name": "e2e-manifests"},
		},
	}}
	ks.SetGroupVersionKind(kustomizationGVK)
	ks.SetName("e2e-payload")
	ks.SetNamespace(namespace)
	if err := c.Create(ctx, ks); err != nil {
		t.Fatalf("creating Kustomization: %v", err)
	}

	waitFor(t, 3*time.Minute, "Flux to reconcile the Kustomization", func() bool {
		var got unstructured.Unstructured
		got.SetGroupVersionKind(kustomizationGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: "e2e-payload", Namespace: namespace}, &got); err != nil {
			return false
		}
		conds, found, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if !found {
			return false
		}
		for _, raw := range conds {
			m, _ := raw.(map[string]any)
			if m["type"] == "Ready" && m["status"] == "True" {
				return true
			}
		}
		return false
	})
}

// TestDeployedControllerCrossesAGate is the whole product, in a real cluster:
// the deployed controller discovers an artifact, admits it, runs a Passage, and
// that Passage's flux-wait blocks on genuine Flux reconciliation rather than a
// fixture.
//
// It is the only test that exercises the controllers as a *deployment* — RBAC,
// the hardened pod, the read-only root filesystem and the scratch volume all
// have to be right for it to pass.
func TestDeployedControllerCrossesAGate(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	freshNamespace(t, c)

	publishArtifact(t, "v1")
	applyFluxSource(t, c, "v1")

	// The cluster's own registry, not the internet. The Beacon runs *in the
	// cluster* here so it cannot reach the test process's loopback, and the k3d
	// registry is plain HTTP — which is exactly what watch[].image.insecure is
	// for (#109). Before that field existed this test pulled from ghcr.io and
	// was the only one in the suite that needed the network.
	const app = "e2e/podinfo"
	publishImages(t, app, "6.0.0", "6.1.0")

	// No spec.watch on the Gate: health must come from what flux-wait emits.
	if err := c.Create(ctx, &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{
				Repo: registryFromCluster + "/" + app, Constraint: "^6.0.0", Insecure: true,
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: namespace},
		Spec: v1alpha1.GateSpec{
			Auto:   true,
			Admits: []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{
				Uses: "flux-wait",
				With: jsonOf(t, map[string]any{
					"resources": []any{map[string]any{"kind": "Kustomization", "name": "e2e-payload"}},
				}),
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The Beacon must discover the image without any help from the test.
	var bundle v1alpha1.Bundle
	waitFor(t, 3*time.Minute, "the deployed Beacon to emit a Bundle", func() bool {
		var list v1alpha1.BundleList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) == 0 {
			return false
		}
		bundle = list.Items[0]
		return true
	})
	img := bundle.Spec.Artifacts[0].Image
	if img.Digest == "" || img.Tag == "" {
		t.Errorf("Bundle did not pin a resolved image: %+v", img)
	}

	// The Gate must cross it on its own, and flux-wait must succeed only
	// because a real Kustomization is Ready.
	waitFor(t, 3*time.Minute, "the Passage to succeed", func() bool {
		var list v1alpha1.PassageList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, p := range list.Items {
			if p.Status.Phase == v1alpha1.PassageSucceeded {
				return true
			}
			if p.Status.Phase == v1alpha1.PassageFailed {
				t.Fatalf("Passage %s failed: %s", p.Name, p.Status.Message)
			}
		}
		return false
	})

	waitFor(t, 2*time.Minute, "the Gate to record the crossing", func() bool {
		var gate v1alpha1.Gate
		if err := c.Get(ctx, types.NamespacedName{Name: "staging", Namespace: namespace}, &gate); err != nil {
			return false
		}
		return gate.Status.Current != nil && gate.Status.Current.Bundle == bundle.Name
	})

	// The durable record downstream Gates read.
	var cleared v1alpha1.Bundle
	if err := c.Get(ctx, types.NamespacedName{Name: bundle.Name, Namespace: namespace}, &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.HasCleared("staging") {
		t.Error("the crossing was not recorded on the Bundle")
	}

	// D20: the Gate declares no spec.watch, so Healthy here can only come from
	// the health check flux-wait handed it. Without that the Gate goes blind
	// the moment the Passage ends.
	waitFor(t, 2*time.Minute, "the Gate to report health adopted from the Passage", func() bool {
		var gate v1alpha1.Gate
		if err := c.Get(ctx, types.NamespacedName{Name: "staging", Namespace: namespace}, &gate); err != nil {
			return false
		}
		return gate.Status.Health != nil && gate.Status.Health.Status == v1alpha1.HealthHealthy
	})
}

// Discovery latency is most of the perceived speed difference against a
// CI-based promotion script (#102). A Beacon's interval is minutes; a CI job
// that has just pushed an image can ask for a poll now.
//
// This has to be an e2e test rather than a unit test, because the whole
// mechanism is the API server's watch: annotating the object is what wakes the
// controller. A unit test calling Reconcile directly proves the acknowledgement
// and nothing about the trigger — and the trigger is one plausible refactor
// away from being lost, since adding GenerationChangedPredicate to the Beacon's
// watch would silently discard every annotation-only change.
func TestAnnotationTriggersAnImmediatePoll(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	freshNamespace(t, c)

	const app = "e2e/poke"
	publishImages(t, app, "6.0.0")

	// An interval long enough that a scheduled poll cannot be mistaken for a
	// triggered one: the next scheduled poll is an hour away, so an
	// acknowledgement arriving at all is the annotation's doing. The waits
	// below are bounded to catch a hang, not to discriminate — the hour does
	// that.
	beacon := &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			Interval: metav1.Duration{Duration: time.Hour},
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{
				Repo: registryFromCluster + "/" + app, Constraint: "^6.0.0", Insecure: true,
			}}},
		},
	}
	mustCreate(t, c, beacon)

	// Let the creation-triggered poll settle first, so what follows can only be
	// the annotation's doing.
	waitFor(t, 3*time.Minute, "the Beacon to poll once", func() bool {
		var b v1alpha1.Beacon
		if err := c.Get(ctx, key(beacon), &b); err != nil {
			return false
		}
		return b.Status.LastPolled != nil
	}, func() string {
		var b v1alpha1.Beacon
		_ = c.Get(ctx, key(beacon), &b)
		return fmt.Sprintf("  lastPolled %v, latestBundle %q",
			b.Status.LastPolled, b.Status.LatestBundle)
	})

	var before v1alpha1.Beacon
	if err := c.Get(ctx, key(beacon), &before); err != nil {
		t.Fatal(err)
	}

	token := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	patched := before.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[v1alpha1.AnnotationReconcile] = token
	if err := c.Patch(ctx, patched, client.MergeFrom(&before)); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Minute, "the Beacon to acknowledge the request", func() bool {
		var b v1alpha1.Beacon
		if err := c.Get(ctx, key(beacon), &b); err != nil {
			return false
		}
		return b.Status.LastHandledReconcileAt == token
	}, func() string {
		var b v1alpha1.Beacon
		_ = c.Get(ctx, key(beacon), &b)
		return fmt.Sprintf("  wanted token %q, status says %q; lastPolled %v",
			token, b.Status.LastHandledReconcileAt, b.Status.LastPolled)
	})

	// Acknowledged *and* actually polled: an echo without a poll would be a
	// controller reporting work it did not do.
	var after v1alpha1.Beacon
	if err := c.Get(ctx, key(beacon), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Status.LastPolled.Time.After(before.Status.LastPolled.Time) {
		t.Errorf("lastPolled did not move (%s -> %s): the request was acknowledged "+
			"without re-polling", before.Status.LastPolled, after.Status.LastPolled)
	}
}

func key(o client.Object) types.NamespacedName {
	return types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}
}

// TestAWaitingPassageDoesNotSpin guards against the controller reconciling a
// waiting Passage as fast as the API server will answer.
//
// It has to be an e2e test, because the bug is invisible to a unit test: the
// engine returns the right RequeueAfter, the controller passes it on, and every
// unit test passes. What went wrong was the watch — a running step's attempt
// count increases on every Advance, so the status write differed every time,
// so it triggered the very watch that had just reconciled. Measured at 113
// reconciles a second against a step that had asked for fifteen seconds.
//
// The threshold is deliberately loose. This is not asserting a precise cadence,
// it is asserting the difference between polling and spinning.
func TestAWaitingPassageDoesNotSpin(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	freshNamespace(t, c)

	// A step that never finishes: flux-wait on a Kustomization nobody creates
	// stays Progressing, which is exactly the state a real crossing sits in
	// while it waits for Flux.
	steps := []v1alpha1.Step{{
		Uses: "flux-wait",
		With: jsonOf(t, map[string]any{"resources": []map[string]any{
			{"kind": "Kustomization", "name": "never-converges", "namespace": namespace},
		}}),
	}}

	mustCreate(t, c, &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: namespace},
		Spec: v1alpha1.BundleSpec{
			Beacon:    "none",
			Artifacts: []v1alpha1.Artifact{{Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/app", Tag: "1.0.0"}}},
		},
	})
	passage := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: namespace},
		Spec:       v1alpha1.PassageSpec{Gate: "staging", Bundle: "b1", Steps: steps},
	}
	mustCreate(t, c, passage)

	waitFor(t, 3*time.Minute, "the Passage to start waiting", func() bool {
		var p v1alpha1.Passage
		if err := c.Get(ctx, key(passage), &p); err != nil {
			return false
		}
		return len(p.Status.Steps) > 0 && p.Status.Steps[0].Phase == v1alpha1.StepRunning
	}, func() string {
		var p v1alpha1.Passage
		_ = c.Get(ctx, key(passage), &p)
		if len(p.Status.Steps) == 0 {
			return "  no steps reported yet"
		}
		return fmt.Sprintf("  step 0 phase is %q, attempts %d",
			p.Status.Steps[0].Phase, p.Status.Steps[0].Attempts)
	})

	attempts := func() int32 {
		var p v1alpha1.Passage
		if err := c.Get(ctx, key(passage), &p); err != nil {
			t.Fatal(err)
		}
		return p.Status.Steps[0].Attempts
	}

	const window = 20 * time.Second
	before := attempts()
	time.Sleep(window)
	after := attempts()

	// flux-wait asks for 15s, so two or three in twenty seconds. Ten allows for
	// scheduling noise and still fails by three orders of magnitude if the
	// controller is spinning.
	const spinning = 10
	if got := after - before; got > spinning {
		t.Errorf("%d reconciles in %s (%d/s) — the controller is spinning rather than "+
			"waiting; a status write is waking the watch that wrote it",
			got, window, int(float64(got)/window.Seconds()))
	}
}

// changesIn counts how many times an observable changes during a window.
//
// The universal symptom of a controller waking itself: a field that moves when
// nothing in the world has. Counting changes rather than reconciles because
// two of the three controllers record a timestamp rather than a counter, and a
// timestamp is what a reconcile leaves behind.
func changesIn(t *testing.T, window time.Duration, read func() string) int {
	t.Helper()
	last := read()
	changes := 0
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if now := read(); now != last {
			changes++
			last = now
		}
	}
	return changes
}

// TestABeaconHonoursItsInterval is the guard for #122.
//
// There is no Gate equivalent, and that is a finding rather than an omission.
// The Gate has the same shape — status.health.observedAt is stamped every
// reconcile — but its reconcile is sub-second, so two of them share a
// timestamp, the write no-ops and the loop cannot start. Measured with the
// Gate's predicate removed: one change in ninety seconds, which is exactly the
// fixed behaviour. A test that passes either way asserts nothing, so the
// Gate's protection is a unit test on the predicate and this comment.
//
// Every Beacon reconcile stamps status.lastPolled with the current time, so
// the write differs from what was there, triggers the Beacon's own watch, and
// it polls again — going back to the registry continuously while ignoring the
// interval it was configured with.
//
// **The source has to be slow, or this test cannot fail.** metav1.Time has
// one-second resolution, so two reconciles inside one second write an
// identical value, the write is a no-op and the loop stops on its own. That is
// a coincidence rather than a safeguard, and it expires exactly when a poll is
// slow — which is when hammering the thing being polled is worst. An
// unroutable address takes tens of seconds to time out, which is what makes
// the bug observable.
func TestABeaconHonoursItsInterval(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireHecateInstalled(t, c)
	freshNamespace(t, c)

	beacon := &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "slow", Namespace: namespace},
		Spec: v1alpha1.BeaconSpec{
			// An hour, so any movement at all is the Beacon waking itself.
			Interval: metav1.Duration{Duration: time.Hour},
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{
				// Non-routable by RFC 5735. Resolving it takes tens of seconds
				// to fail, so no two reconciles share a timestamp.
				Repo: "10.255.255.1:5000/acme/app", Insecure: true,
			}}},
		},
	}
	mustCreate(t, c, beacon)

	// The first poll has to finish before the measurement means anything: it
	// is a legitimate change, and it takes as long as the timeout.
	waitFor(t, 3*time.Minute, "the Beacon's first poll to finish", func() bool {
		var b v1alpha1.Beacon
		if err := c.Get(ctx, key(beacon), &b); err != nil {
			return false
		}
		return b.Status.LastPolled != nil
	})

	const window = 150 * time.Second
	changes := changesIn(t, window, func() string {
		var b v1alpha1.Beacon
		if err := c.Get(ctx, key(beacon), &b); err != nil {
			return ""
		}
		if b.Status.LastPolled == nil {
			return ""
		}
		return b.Status.LastPolled.String()
	})

	// One allows for a poll that was already in flight when the window opened.
	// Unfixed, this is two or three: each poll takes about as long as it takes
	// the address to time out, and the next starts immediately.
	if changes > 1 {
		t.Errorf("the Beacon polled %d more times in %s despite a one-hour interval — "+
			"its own status write is waking it, and every one of those is a request "+
			"to a registry that is already slow", changes, window)
	}
}
