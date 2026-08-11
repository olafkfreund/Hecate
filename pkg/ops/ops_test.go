package ops

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var base = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func newOps(t *testing.T, objs ...client.Object) (*Ops, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{}).
		Build()
	return &Ops{Client: c, Now: func() metav1.Time { return metav1.Time{Time: base} }}, c
}

func testGate(name string, opts ...func(*v1alpha1.Gate)) *v1alpha1.Gate {
	g := &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Admits:  []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{Uses: "flux-wait"}}},
		},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func testBundle(name string, ageMinutes int, opts ...func(*v1alpha1.Bundle)) *v1alpha1.Bundle {
	b := &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "acme",
			CreationTimestamp: metav1.Time{Time: base.Add(time.Duration(ageMinutes) * time.Minute)},
		},
		Spec: v1alpha1.BundleSpec{Beacon: "app"},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func TestReadsAreOrdered(t *testing.T) {
	o, _ := newOps(t,
		testGate("production"), testGate("dev"), testGate("staging"),
		testBundle("old", -60), testBundle("newest", 0), testBundle("middle", -30),
	)
	ctx := context.Background()

	gates, err := o.Gates(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// By name: a list that prints differently on each read is not diffable.
	if got := []string{gates[0].Name, gates[1].Name, gates[2].Name}; got[0] != "dev" || got[2] != "staging" {
		t.Errorf("gates = %v, want alphabetical", got)
	}

	bundles, err := o.Bundles(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: the question is nearly always about the recent ones.
	if bundles[0].Name != "newest" || bundles[2].Name != "old" {
		t.Errorf("bundles = %s, %s, %s", bundles[0].Name, bundles[1].Name, bundles[2].Name)
	}
}

// Every surface answers "not there" differently — a 404, an exit code, a
// message a model can act on — so it must be distinguishable.
func TestMissingThingsAreNamed(t *testing.T) {
	o, _ := newOps(t)
	ctx := context.Background()

	for _, err := range []error{
		second(o.Gate(ctx, "acme", "ghost")),
		second(o.Bundle(ctx, "acme", "ghost")),
		second(o.Passage(ctx, "acme", "ghost")),
	} {
		if !IsNotFound(err) {
			t.Errorf("err = %v, want a NotFoundError", err)
		}
		if !strings.Contains(err.Error(), "ghost") {
			t.Errorf("the error does not name what was missing: %v", err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestPassagesFilter(t *testing.T) {
	o, _ := newOps(t,
		passageFor("staging", "b1", v1alpha1.PassageSucceeded),
		passageFor("production", "b1", v1alpha1.PassageRunning),
		passageFor("staging", "b2", v1alpha1.PassageFailed),
	)

	all, err := o.Passages(context.Background(), "acme", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("got %d Passages, want 3", len(all))
	}

	staging, err := o.Passages(context.Background(), "acme", "staging", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 2 {
		t.Errorf("staging has %d Passages, want 2", len(staging))
	}

	b1, err := o.Passages(context.Background(), "acme", "", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b1) != 2 {
		t.Errorf("b1 has %d Passages, want 2", len(b1))
	}
}

func passageFor(gate, bundle string, phase v1alpha1.PassagePhase) *v1alpha1.Passage {
	return &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{
			Name: gate + "-" + bundle, Namespace: "acme",
			CreationTimestamp: metav1.Time{Time: base},
		},
		Spec:   v1alpha1.PassageSpec{Gate: gate, Bundle: bundle, Steps: []v1alpha1.Step{{Uses: "flux-wait"}}},
		Status: v1alpha1.PassageStatus{Phase: phase},
	}
}
