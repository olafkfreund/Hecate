package ops

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func testBeacon(name string, opts ...func(*v1alpha1.Beacon)) *v1alpha1.Beacon {
	b := &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec: v1alpha1.BeaconSpec{
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{Repo: "ghcr.io/acme/app"}}},
		},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func suspended(b *v1alpha1.Beacon) { b.Spec.Suspend = true }

// The whole of the webhook endpoint: a git host cannot set a Kubernetes
// annotation, and the annotation is what the Beacon already reacts to.
func TestPollSetsTheAnnotationTheBeaconReactsTo(t *testing.T) {
	o, c := newOps(t, testBeacon("app"))

	token, err := o.Poll(context.Background(), "acme", "app")
	if err != nil {
		t.Fatal(err)
	}

	var b v1alpha1.Beacon
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "acme", Name: "app"}, &b); err != nil {
		t.Fatal(err)
	}
	// Flux's own annotation, not one of ours: tooling that already pokes Flux
	// resources works on a Beacon unchanged (D44).
	got := b.Annotations[v1alpha1.AnnotationReconcile]
	if got == "" {
		t.Fatal("no reconcile annotation, so the Beacon waits out its interval and the endpoint does nothing")
	}
	// Echoed back so a caller can match it against status.lastHandledReconcileAt
	// and know its own request landed rather than someone else's.
	if got != token {
		t.Errorf("annotation = %q but the caller was told %q", got, token)
	}
}

// A Beacon poked twice must see two distinct values, or the second request
// looks like the first arriving again and no reconcile is triggered.
func TestTwoPollsAreDistinguishable(t *testing.T) {
	o, _ := newOps(t, testBeacon("app"))
	// A real clock: the shared harness freezes time, which would make this pass
	// for the wrong reason by making both tokens equal and the test blind.
	o.Now = func() metav1.Time { return metav1.Now() }
	ctx := context.Background()

	first, err := o.Poll(ctx, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.Poll(ctx, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("both polls used %q — the second is indistinguishable from a retry of the first", first)
	}
}

// A suspended Beacon would acknowledge the request and poll nothing, which
// reads as success to whatever called the webhook.
func TestPollingASuspendedBeaconIsRefused(t *testing.T) {
	o, c := newOps(t, testBeacon("app", suspended))

	_, err := o.Poll(context.Background(), "acme", "app")
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal naming the suspension", err)
	}

	var b v1alpha1.Beacon
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "acme", Name: "app"}, &b); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Annotations[v1alpha1.AnnotationReconcile]; ok {
		t.Error("a suspended Beacon was annotated, so it will poll the moment it is resumed")
	}
}

func TestPollingAMissingBeaconIsAnError(t *testing.T) {
	o, _ := newOps(t, testBeacon("app"))
	if _, err := o.Poll(context.Background(), "acme", "nope"); err == nil {
		t.Error("polling a Beacon that does not exist succeeded")
	}
}

// Existing annotations must survive: a merge patch on one key, not a rewrite of
// the map, or a poll would strip whatever else is on the object.
func TestPollKeepsOtherAnnotations(t *testing.T) {
	b := testBeacon("app")
	b.Annotations = map[string]string{"acme.example/owner": "platform"}
	o, c := newOps(t, b)

	if _, err := o.Poll(context.Background(), "acme", "app"); err != nil {
		t.Fatal(err)
	}

	var got v1alpha1.Beacon
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "acme", Name: "app"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations["acme.example/owner"] != "platform" {
		t.Errorf("annotations = %v — polling removed one it did not set", got.Annotations)
	}
}
