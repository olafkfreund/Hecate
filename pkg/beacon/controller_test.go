package beacon

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var clock = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func beaconWith(watches ...v1alpha1.WatchSource) *v1alpha1.Beacon {
	return &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "acme", Generation: 1},
		Spec:       v1alpha1.BeaconSpec{Watch: watches},
	}
}

func imageWatch(repo string) v1alpha1.WatchSource {
	return v1alpha1.WatchSource{Image: &v1alpha1.ImageWatch{Repo: repo}}
}

func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Beacon{}).
		Build()
	rec := record.NewFakeRecorder(20)
	return &Reconciler{
		Client:   c,
		Resolver: &Resolver{Client: c},
		Recorder: rec,
		Now:      func() time.Time { return clock },
	}, c, rec
}

func reconcile(t *testing.T, r *Reconciler, b *v1alpha1.Beacon) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getBeacon(t *testing.T, c client.Client) *v1alpha1.Beacon {
	t.Helper()
	var b v1alpha1.Beacon
	if err := c.Get(context.Background(), types.NamespacedName{Name: "podinfo", Namespace: "acme"}, &b); err != nil {
		t.Fatal(err)
	}
	return &b
}

func listBundles(t *testing.T, c client.Client) []v1alpha1.Bundle {
	t.Helper()
	var l v1alpha1.BundleList
	if err := c.List(context.Background(), &l, client.InNamespace("acme")); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func readyCondition(t *testing.T, b *v1alpha1.Beacon) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(b.Status.Conditions, v1alpha1.ConditionReady)
}

// The core requirement of #58: rediscovering the same release must not mint a
// second Bundle, no matter how many times we poll.
func TestReconcileIsIdempotent(t *testing.T) {
	repo, digests := pushTags(t, "acme/podinfo", "6.1.0", "6.2.0")
	b := beaconWith(imageWatch(repo))
	r, c, _ := newReconciler(t, b)

	for i := 1; i <= 3; i++ {
		reconcile(t, r, b)
		bundles := listBundles(t, c)
		if len(bundles) != 1 {
			t.Fatalf("after %d reconciles: %d Bundles, want exactly 1", i, len(bundles))
		}
	}

	bundle := listBundles(t, c)[0]
	if bundle.Spec.Artifacts[0].Image.Digest != digests["6.2.0"] {
		t.Errorf("Bundle pinned the wrong digest: %s", bundle.Spec.Artifacts[0].Image.Digest)
	}
	if bundle.Labels[LabelBeacon] != "podinfo" {
		t.Errorf("Bundle not labelled with its Beacon: %v", bundle.Labels)
	}
	// An owner reference would cascade-delete the audit trail with the Beacon.
	if len(bundle.OwnerReferences) != 0 {
		t.Errorf("Bundle must not have owner references, got %v", bundle.OwnerReferences)
	}
}

func TestReconcileEmitsOnChange(t *testing.T) {
	repo := newTestRepo(t, "acme/podinfo", "6.1.0")
	b := beaconWith(imageWatch(repo.Repo))
	r, c, _ := newReconciler(t, b)

	reconcile(t, r, b)
	if n := len(listBundles(t, c)); n != 1 {
		t.Fatalf("got %d Bundles, want 1", n)
	}

	// A new release is published to the same repository — no spec change.
	repo.Push("6.2.0")

	reconcile(t, r, b)
	bundles := listBundles(t, c)
	if len(bundles) != 2 {
		t.Fatalf("got %d Bundles, want 2 after a new release", len(bundles))
	}

	latest := getBeacon(t, c).Status.LatestBundle
	var found bool
	for _, bn := range bundles {
		if bn.Name == latest && bn.Spec.Artifacts[0].Image.Digest == repo.Digests["6.2.0"] {
			found = true
		}
	}
	if !found {
		t.Errorf("latestBundle %q does not point at the new release", latest)
	}
}

// status.discovered must be populated even when nothing is emitted — it is what
// answers "why has nothing appeared?".
func TestDiscoveredIsRecordedWithoutEmitting(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")

	t.Run("emit policy Manual", func(t *testing.T) {
		b := beaconWith(imageWatch(repo))
		b.Spec.Emit = v1alpha1.EmitManual
		r, c, _ := newReconciler(t, b)

		reconcile(t, r, b)

		if n := len(listBundles(t, c)); n != 0 {
			t.Errorf("Manual emit created %d Bundles, want 0", n)
		}
		got := getBeacon(t, c)
		if len(got.Status.Discovered) != 1 {
			t.Fatalf("discovered = %d artifacts, want 1", len(got.Status.Discovered))
		}
		if got.Status.Discovered[0].Image.Tag != "6.1.0" {
			t.Errorf("discovered the wrong tag: %s", got.Status.Discovered[0].Image.Tag)
		}
		if cond := readyCondition(t, got); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("Manual emit should still be Ready, got %+v", cond)
		}
	})

	t.Run("nothing matches the constraint", func(t *testing.T) {
		b := beaconWith(v1alpha1.WatchSource{
			Image: &v1alpha1.ImageWatch{Repo: repo, Constraint: "^99.0.0"},
		})
		r, c, _ := newReconciler(t, b)

		reconcile(t, r, b)

		if n := len(listBundles(t, c)); n != 0 {
			t.Errorf("created %d Bundles despite no match, want 0", n)
		}
		cond := readyCondition(t, getBeacon(t, c))
		if cond == nil || cond.Reason != "NoMatchingArtifact" {
			t.Errorf("want reason NoMatchingArtifact, got %+v", cond)
		}
	})
}

// A Bundle missing one of its artifacts would promote an incomplete set.
func TestPartialResolutionEmitsNothing(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")
	b := beaconWith(
		imageWatch(repo),
		v1alpha1.WatchSource{Git: &v1alpha1.GitWatch{Repo: "https://github.com/acme/app", Branch: "main"}},
	)
	r, c, _ := newReconciler(t, b)

	reconcile(t, r, b)

	if n := len(listBundles(t, c)); n != 0 {
		t.Fatalf("created %d Bundles from a partial resolution, want 0", n)
	}
	got := getBeacon(t, c)
	cond := readyCondition(t, got)
	if cond == nil {
		t.Fatal("expected a Ready condition explaining why nothing was emitted")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("partial resolution should not be Ready, got %+v", cond)
	}
	if cond.Reason != "UnsupportedSource" {
		t.Errorf("want reason UnsupportedSource, got %q", cond.Reason)
	}
	// The image that did resolve is still reported.
	if len(got.Status.Discovered) != 1 {
		t.Errorf("discovered = %d, want the one source that resolved", len(got.Status.Discovered))
	}
}

func TestSuspendStopsDiscovery(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")
	b := beaconWith(imageWatch(repo))
	b.Spec.Suspend = true
	r, c, _ := newReconciler(t, b)

	res := reconcile(t, r, b)

	if n := len(listBundles(t, c)); n != 0 {
		t.Errorf("suspended Beacon emitted %d Bundles, want 0", n)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("suspended Beacon should not requeue, got %s", res.RequeueAfter)
	}
	cond := readyCondition(t, getBeacon(t, c))
	if cond == nil || cond.Reason != "Suspended" {
		t.Errorf("want reason Suspended, got %+v", cond)
	}
}

func TestRequeuesAtTheConfiguredInterval(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")

	b := beaconWith(imageWatch(repo))
	b.Spec.Interval = metav1.Duration{Duration: 90 * time.Second}
	r, _, _ := newReconciler(t, b)
	if res := reconcile(t, r, b); res.RequeueAfter != 90*time.Second {
		t.Errorf("RequeueAfter = %s, want 90s", res.RequeueAfter)
	}

	b2 := beaconWith(imageWatch(repo))
	b2.Name = "podinfo" // same name, fresh client below
	r2, _, _ := newReconciler(t, b2)
	if res := reconcile(t, r2, b2); res.RequeueAfter != DefaultInterval {
		t.Errorf("RequeueAfter = %s, want the default %s", res.RequeueAfter, DefaultInterval)
	}
}

func TestEmittingRecordsAnEvent(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")
	b := beaconWith(imageWatch(repo))
	r, _, rec := newReconciler(t, b)

	reconcile(t, r, b)
	select {
	case e := <-rec.Events:
		if !contains(e, "BundleEmitted") {
			t.Errorf("event = %q, want a BundleEmitted event", e)
		}
	default:
		t.Error("emitting a Bundle should record an event for notification-controller")
	}

	// A steady-state poll must not spam events.
	reconcile(t, r, b)
	select {
	case e := <-rec.Events:
		t.Errorf("unchanged poll recorded an event: %q", e)
	default:
	}
}

func TestMissingBeaconIsNotAnError(t *testing.T) {
	r, _, _ := newReconciler(t)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "acme"},
	})
	if err != nil {
		t.Errorf("a deleted Beacon should reconcile cleanly, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
