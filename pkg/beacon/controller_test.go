package beacon

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/controller-runtime/pkg/event"

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

func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Beacon{}).
		Build()
	rec := events.NewFakeRecorder(20)
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
	// Platform digest resolution is the remaining declared-but-unimplemented
	// option; git watches used to stand in here and resolve now (#106).
	b := beaconWith(
		imageWatch(repo),
		v1alpha1.WatchSource{Image: &v1alpha1.ImageWatch{
			Repo: repo, Platform: "linux/arm64",
		}},
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

// A Beacon polls on every reconcile with no interval gate, so an annotation
// change is already enough to trigger an immediate poll. What makes that a
// contract rather than an accident is the acknowledgement: without it a caller
// knows only that *a* reconcile happened, and a CI job asking for an immediate
// poll has nothing to wait for.
func TestBeaconAcknowledgesAReconcileRequest(t *testing.T) {
	repo := newTestRepo(t, "acme/podinfo", "6.1.0")
	b := beaconWith(imageWatch(repo.Repo))
	b.Annotations = map[string]string{v1alpha1.AnnotationReconcile: "1755000000"}
	r, c, _ := newReconciler(t, b)

	reconcile(t, r, b)

	got := getBeacon(t, c)
	if got.Status.LastHandledReconcileAt != "1755000000" {
		t.Errorf("lastHandledReconcileAt = %q, want the caller's own token echoed back",
			got.Status.LastHandledReconcileAt)
	}
	// The poll itself must have happened, or the acknowledgement is a lie.
	if got.Status.LastPolled == nil {
		t.Error("acknowledged a reconcile request without polling")
	}
}

// A suspended Beacon has nothing to poll, but leaving the request unhandled
// for ever would make a caller wait on something that is never coming.
func TestSuspendedBeaconStillAcknowledges(t *testing.T) {
	b := beaconWith(imageWatch("example.invalid/acme/podinfo"))
	b.Spec.Suspend = true
	b.Annotations = map[string]string{v1alpha1.AnnotationReconcile: "abc"}
	r, c, _ := newReconciler(t, b)

	reconcile(t, r, b)

	if got := getBeacon(t, c).Status.LastHandledReconcileAt; got != "abc" {
		t.Errorf("lastHandledReconcileAt = %q, want %q even while suspended", got, "abc")
	}
}

// The Beacon's own status write must not wake it.
//
// Every reconcile sets status.lastPolled to now, so the write differs from what
// was there and triggers the watch — the Beacon then polls again, ignoring its
// interval. Hidden by metav1.Time's one-second resolution until a poll takes
// longer than a second, which is a slow registry, which is exactly when
// re-polling it continuously is worst.
func TestABeaconIsNotWokenByItsOwnStatusWrite(t *testing.T) {
	p := pollTrigger()

	old := &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "acme", Generation: 1},
	}
	// Only the status changed, as the controller's own write does.
	updated := old.DeepCopy()
	updated.Status.LastPolled = &metav1.Time{Time: clock}
	updated.ResourceVersion = "2"

	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Error("a status-only change woke the Beacon — it will poll continuously " +
			"and ignore the interval it was configured with")
	}
}

// The annotation is the whole of the webhook endpoint (#102), so a predicate
// that discarded it would leave the trigger silently dead.
func TestAnAnnotationStillWakesTheBeacon(t *testing.T) {
	p := pollTrigger()

	old := &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "acme", Generation: 1},
	}
	poked := old.DeepCopy()
	poked.Annotations = map[string]string{v1alpha1.AnnotationReconcile: "1786620716793895315"}

	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: poked}) {
		t.Error("a poll request did not wake the Beacon, so `hecate poll` and the " +
			"webhook endpoint do nothing")
	}

	// And a second, different token must wake it again: two pokes within one
	// second have to be distinguishable, which is why the token is nanoseconds.
	again := poked.DeepCopy()
	again.Annotations[v1alpha1.AnnotationReconcile] = "1786620716793895999"
	if !p.Update(event.UpdateEvent{ObjectOld: poked, ObjectNew: again}) {
		t.Error("a second poll request was ignored")
	}
}

// A spec change is the other reason to look at the world again.
func TestASpecChangeWakesTheBeacon(t *testing.T) {
	p := pollTrigger()
	old := &v1alpha1.Beacon{ObjectMeta: metav1.ObjectMeta{Name: "app", Generation: 1}}
	edited := old.DeepCopy()
	edited.Generation = 2

	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: edited}) {
		t.Error("editing a Beacon's spec did not wake it")
	}
}
