package gate

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// aPassage is a finished Passage for the `staging` Gate, ageMinutes from base.
func aPassage(name string, ageMinutes int, phase v1alpha1.PassagePhase) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "acme",
			Labels:            map[string]string{LabelGate: "staging", LabelBundle: "app-1"},
			CreationTimestamp: metav1.Time{Time: base.Add(time.Duration(ageMinutes) * time.Minute)},
		},
		Spec:   v1alpha1.PassageSpec{Gate: "staging", Bundle: "app-1"},
		Status: v1alpha1.PassageStatus{Phase: phase},
	}
}

// manyPassages is n finished Passages, oldest first.
func manyPassages(n int, phase v1alpha1.PassagePhase) []client.Object {
	objs := make([]client.Object, 0, n)
	for i := 0; i < n; i++ {
		objs = append(objs, aPassage(fmt.Sprintf("p-%02d", i), i, phase))
	}
	return objs
}

func passageNames(t *testing.T, c client.Client) []string {
	t.Helper()
	var list v1alpha1.PassageList
	if err := c.List(context.Background(), &list, client.InNamespace("acme")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names
}

// retaining runs collect directly, so the tests are about the retention rules
// rather than about everything else a reconcile does.
func retaining(t *testing.T, gate *v1alpha1.Gate, objs ...client.Object) (int, client.Client) {
	t.Helper()
	r, c, _ := newReconciler(t, append([]client.Object{gate}, objs...)...)
	deleted, err := r.collect(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	return deleted, c
}

func TestCollectKeepsTheNewestUpToTheLimit(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(3))

	deleted, c := retaining(t, gate, manyPassages(10, v1alpha1.PassageSucceeded)...)

	if deleted != 7 {
		t.Errorf("deleted %d, want 7", deleted)
	}
	// p-07..p-09 are the newest three.
	got := passageNames(t, c)
	want := []string{"p-07", "p-08", "p-09"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// Zero is "keep everything". An unset-looking field must not destroy history —
// the opposite reading turns a typo into data loss.
func TestRetainZeroKeepsEverything(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(0))

	deleted, c := retaining(t, gate, manyPassages(30, v1alpha1.PassageSucceeded)...)

	if deleted != 0 {
		t.Errorf("deleted %d with retain=0", deleted)
	}
	if len(passageNames(t, c)) != 30 {
		t.Errorf("kept %d of 30", len(passageNames(t, c)))
	}
}

func TestUnsetRetainUsesTheDefault(t *testing.T) {
	gate := gateAdmitting("staging")

	deleted, c := retaining(t, gate, manyPassages(int(DefaultRetain)+5, v1alpha1.PassageSucceeded)...)

	if deleted != 5 {
		t.Errorf("deleted %d, want 5", deleted)
	}
	if got := len(passageNames(t, c)); got != int(DefaultRetain) {
		t.Errorf("kept %d, want %d", got, DefaultRetain)
	}
}

// The first safety rule. A Passage still running is work in flight: deleting it
// abandons a crossing part-way and leaves the Gate waiting on an object that no
// longer exists.
func TestUnfinishedPassagesAreNeverCollected(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(1))

	objs := []client.Object{
		aPassage("running", 0, v1alpha1.PassageRunning),
		aPassage("pending", 1, v1alpha1.PassagePending),
	}
	objs = append(objs, manyPassages(5, v1alpha1.PassageFailed)...)

	_, c := retaining(t, gate, objs...)

	got := passageNames(t, c)
	for _, name := range []string{"running", "pending"} {
		if !contains(got, name) {
			t.Errorf("%s was collected — a crossing in flight was deleted. kept: %v", name, got)
		}
	}
}

// The second safety rule, and the one that matters most: the Passage that put
// the current occupant into the Gate is the record of how what is running got
// there. A tool whose product is the audit trail must not delete it for space.
func TestThePassageBehindTheCurrentOccupantSurvives(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(1))
	gate.Status.Current = &v1alpha1.GateOccupant{
		Bundle: "app-1", Passage: "p-00", EnteredAt: metav1.Time{Time: base},
	}

	_, c := retaining(t, gate, manyPassages(10, v1alpha1.PassageSucceeded)...)

	// p-00 is the oldest, so ordering alone would have collected it first.
	if got := passageNames(t, c); !contains(got, "p-00") {
		t.Fatalf("the Passage behind the current occupant was collected. kept: %v", got)
	}
}

// History names the Passage behind each previous occupant — the rollback
// targets someone reads when a release has gone wrong, which is exactly when
// they must still be there.
func TestPassagesBehindPreviousOccupantsSurvive(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(1))
	gate.Status.History = []v1alpha1.GateOccupant{
		{Bundle: "app-1", Passage: "p-01", EnteredAt: metav1.Time{Time: base}},
		{Bundle: "app-1", Passage: "p-02", EnteredAt: metav1.Time{Time: base}},
	}

	_, c := retaining(t, gate, manyPassages(10, v1alpha1.PassageSucceeded)...)

	got := passageNames(t, c)
	for _, name := range []string{"p-01", "p-02"} {
		if !contains(got, name) {
			t.Errorf("%s is a rollback target and was collected. kept: %v", name, got)
		}
	}
}

// The active Passage is protected by name rather than by being newest: the
// controller lists from an informer cache that may not have a just-created
// Passage yet, and creation timestamps come from the API server. Ordering alone
// would make this a create-delete loop.
func TestTheActivePassageIsProtectedByName(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(1))
	gate.Status.ActivePassage = "p-00"

	_, c := retaining(t, gate, manyPassages(10, v1alpha1.PassageSucceeded)...)

	if got := passageNames(t, c); !contains(got, "p-00") {
		t.Fatalf("the active Passage was collected. kept: %v", got)
	}
}

// Another Gate's Passages are not this Gate's to collect.
func TestCollectionIsScopedToTheGate(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(1))

	other := aPassage("other-gate", 0, v1alpha1.PassageSucceeded)
	other.Labels[LabelGate] = "production"
	other.Spec.Gate = "production"

	objs := append([]client.Object{other}, manyPassages(5, v1alpha1.PassageSucceeded)...)
	_, c := retaining(t, gate, objs...)

	if got := passageNames(t, c); !contains(got, "other-gate") {
		t.Fatalf("collected a Passage belonging to another Gate. kept: %v", got)
	}
}

// #108's premise: a Passage protects its Bundle from collection, so unbounded
// Passages meant unbounded Bundles. Collection has to actually reduce the count
// for Bundle collection (D13) to be able to make progress.
func TestCollectionUnblocksBundleCollection(t *testing.T) {
	gate := gateAdmitting("staging")
	gate.Spec.Retain = ptr(int32(2))

	before := 12
	deleted, c := retaining(t, gate, manyPassages(before, v1alpha1.PassageSucceeded)...)

	after := len(passageNames(t, c))
	if after >= before {
		t.Fatalf("collection removed nothing: %d before, %d after", before, after)
	}
	if deleted != before-after {
		t.Errorf("reported %d deleted but %d disappeared", deleted, before-after)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }

// Through a real reconcile rather than calling collect directly, because the
// wiring is where this could silently do nothing: collect runs after advance
// and before the status update, and a reconcile that returns early would skip
// it without any test noticing.
func TestReconcileCollects(t *testing.T) {
	gate := autoGate("staging", v1alpha1.Admission{From: v1alpha1.BundleOrigin{Beacon: "app"}})
	gate.Spec.Retain = ptr(int32(2))

	objs := append([]client.Object{gate}, manyPassages(9, v1alpha1.PassageSucceeded)...)
	r, c, _ := newReconciler(t, objs...)

	reconcileGate(t, r, "staging")

	kept := passageNames(t, c)
	// The reconcile may open a new Passage of its own; what matters is that the
	// nine finished ones were reduced to the limit.
	var finished int
	var list v1alpha1.PassageList
	if err := c.List(context.Background(), &list, client.InNamespace("acme")); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		if list.Items[i].Status.Phase.Terminal() {
			finished++
		}
	}
	if finished > 2 {
		t.Errorf("reconcile left %d finished Passages, want at most 2. all: %v", finished, kept)
	}
}
