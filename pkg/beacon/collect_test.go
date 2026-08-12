package beacon

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func aBundle(name string, ageMinutes int) *v1alpha1.Bundle {
	return &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "acme",
			Labels:            map[string]string{LabelBeacon: "podinfo"},
			CreationTimestamp: metav1.Time{Time: clock.Add(time.Duration(ageMinutes) * time.Minute)},
		},
		Spec: v1alpha1.BundleSpec{Beacon: "podinfo"},
	}
}

func collector(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Beacon{}, &v1alpha1.Bundle{}, &v1alpha1.Gate{}).
		Build()
	return &Reconciler{Client: c, Now: func() time.Time { return clock }}, c
}

func bundleNames(t *testing.T, c client.Client) []string {
	t.Helper()
	var l v1alpha1.BundleList
	if err := c.List(context.Background(), &l, client.InNamespace("acme")); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(l.Items))
	for _, b := range l.Items {
		out = append(out, b.Name)
	}
	return out
}

func retaining(n int32) *v1alpha1.Beacon {
	b := beaconWith()
	b.Spec.Retain = &n
	return b
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestCollectKeepsTheNewest(t *testing.T) {
	var objs []client.Object
	for i := 0; i < 8; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("b%d", i), i)) // b7 newest
	}
	beacon := retaining(3)
	objs = append(objs, beacon)
	r, c := collector(t, objs...)

	deleted, err := r.collect(context.Background(), beacon)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 5 {
		t.Errorf("deleted %d, want 5", deleted)
	}

	got := bundleNames(t, c)
	if len(got) != 3 {
		t.Fatalf("kept %d Bundles (%v), want 3", len(got), got)
	}
	for _, want := range []string{"b7", "b6", "b5"} {
		if !has(got, want) {
			t.Errorf("collected %s — the newest must survive; kept %v", want, got)
		}
	}
}

func TestCollectBelowTheLimitDoesNothing(t *testing.T) {
	beacon := retaining(10)
	r, c := collector(t, beacon, aBundle("b1", 0), aBundle("b2", 1))

	deleted, err := r.collect(context.Background(), beacon)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || len(bundleNames(t, c)) != 2 {
		t.Errorf("deleted %d with only 2 Bundles and a limit of 10", deleted)
	}
}

// The safety rule. Deleting the record of what is running in production to save
// etcd space would be a spectacular own goal.
func TestCollectNeverTouchesBundlesInUse(t *testing.T) {
	current := aBundle("in-production", 0) // oldest, so first in line to go
	approved := aBundle("approved", 1)
	approved.Status.ApprovedFor = []v1alpha1.BundleApproval{{Gate: "production", Actor: "olaf@acme.example"}}
	crossed := aBundle("has-a-passage", 2)

	gate := &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Admits: []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "podinfo"}}},
		},
		Status: v1alpha1.GateStatus{
			Current: &v1alpha1.GateOccupant{Bundle: "in-production"},
		},
	}
	passage := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "has-a-passage"},
	}

	objs := []client.Object{current, approved, crossed, gate, passage}
	for i := 0; i < 6; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("junk%d", i), 10+i))
	}
	// Retain 1: everything unreferenced beyond the newest must go, and every
	// referenced Bundle must survive anyway.
	beacon := retaining(1)
	objs = append(objs, beacon)
	r, c := collector(t, objs...)

	if _, err := r.collect(context.Background(), beacon); err != nil {
		t.Fatal(err)
	}

	got := bundleNames(t, c)
	for _, protected := range []string{"in-production", "approved", "has-a-passage"} {
		if !has(got, protected) {
			t.Errorf("collected %s, which is in use — kept %v", protected, got)
		}
	}
	// 3 protected + 1 retained unreferenced.
	if len(got) != 4 {
		t.Errorf("kept %d Bundles (%v), want 4", len(got), got)
	}
}

// Zero means "keep everything", not "keep none" — reading it the other way
// would make an unset-looking field destroy history.
func TestRetainZeroDisablesCollection(t *testing.T) {
	objs := []client.Object{retaining(0)}
	for i := 0; i < 5; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("b%d", i), i))
	}
	r, c := collector(t, objs...)

	deleted, err := r.collect(context.Background(), objs[0].(*v1alpha1.Beacon))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || len(bundleNames(t, c)) != 5 {
		t.Errorf("retain=0 deleted %d Bundles, want 0", deleted)
	}
}

func TestCollectDefaultsToTen(t *testing.T) {
	objs := []client.Object{beaconWith()} // no Retain set
	for i := 0; i < 15; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("b%02d", i), i))
	}
	r, c := collector(t, objs...)

	if _, err := r.collect(context.Background(), objs[0].(*v1alpha1.Beacon)); err != nil {
		t.Fatal(err)
	}
	if got := len(bundleNames(t, c)); got != int(DefaultRetain) {
		t.Errorf("kept %d Bundles, want the default %d", got, DefaultRetain)
	}
}

// Another Beacon's Bundles are not this Beacon's to collect.
func TestCollectIsScopedToItsOwnBeacon(t *testing.T) {
	other := aBundle("other-service", 0)
	other.Labels[LabelBeacon] = "other"
	other.Spec.Beacon = "other"

	objs := []client.Object{retaining(1), other}
	for i := 0; i < 4; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("mine%d", i), 10+i))
	}
	r, c := collector(t, objs...)

	if _, err := r.collect(context.Background(), objs[0].(*v1alpha1.Beacon)); err != nil {
		t.Fatal(err)
	}
	if !has(bundleNames(t, c), "other-service") {
		t.Error("collected a Bundle belonging to a different Beacon")
	}
}

// Collection runs as part of the normal loop, and a Bundle just emitted must
// never be the one collected.
func TestReconcileCollectsAndSparesTheFreshBundle(t *testing.T) {
	repo, _ := pushTags(t, "acme/podinfo", "6.1.0")
	beacon := beaconWith(imageWatch(repo))
	beacon.Spec.Retain = func() *int32 { n := int32(1); return &n }()

	objs := []client.Object{beacon}
	for i := 0; i < 4; i++ {
		objs = append(objs, aBundle(fmt.Sprintf("old%d", i), i))
	}
	r, c, _ := newReconciler(t, objs...)

	reconcile(t, r, beacon)

	got := bundleNames(t, c)
	latest := getBeacon(t, c).Status.LatestBundle
	if !has(got, latest) {
		t.Errorf("the Bundle just emitted (%s) was collected; kept %v", latest, got)
	}
	// retain=1 bounds the *unreferenced* Bundles; the freshly emitted one is
	// protected on top of that, so two survive.
	if len(got) != 2 {
		t.Errorf("kept %d Bundles (%v), want 2: the fresh Bundle plus one retained", len(got), got)
	}
}

func TestCollectSurvivesAMissingBundle(t *testing.T) {
	// A concurrent delete must not fail the whole collection.
	beacon := retaining(1)
	b1, b2 := aBundle("b1", 0), aBundle("b2", 1)
	r, c := collector(t, beacon, b1, b2)

	if err := c.Delete(context.Background(), &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.collect(context.Background(), beacon); err != nil {
		t.Errorf("collection failed on an already-deleted Bundle: %v", err)
	}
	var remaining v1alpha1.Bundle
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "b2", Namespace: "acme"}, &remaining); err != nil {
		t.Errorf("the retained Bundle went missing: %v", err)
	}
}
