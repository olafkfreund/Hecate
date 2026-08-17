package gate

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// capturing is an events.EventRecorder that keeps the `action` argument.
//
// events.FakeRecorder is used everywhere else here, but it formats an event as
// type + reason + note and throws `action` away — so with it alone, every
// action in the codebase could be empty or wrong and every test would still
// pass. This keeps the one field the shared fake drops.
type capturing struct {
	events []recorded
}

type recorded struct{ eventType, reason, action string }

func (c *capturing) Eventf(_, _ runtime.Object, eventType, reason, action, _ string, _ ...any) {
	c.events = append(c.events, recorded{eventType, reason, action})
}

func (c *capturing) find(reason string) (recorded, bool) {
	for _, e := range c.events {
		if e.reason == reason {
			return e, true
		}
	}
	return recorded{}, false
}

// The vocabulary, pinned against the real call sites.
//
// `action` is what the controller did, `reason` is how it turned out — the
// split the events API asks for, and why this migration was not a rename
// (#116). These strings are what people write alerts against, so changing one
// is a change to somebody's paging rule rather than a refactor. If you are
// here because this failed, that is the question to answer before you update
// the expectation.
//
// Driven through Reconcile rather than by calling r.event directly. A table
// that passes its own literals in and asserts them back proves only that Go
// can pass arguments — the first version of this test did exactly that, and a
// deliberately wrong action at the call site sailed straight through it.
func TestGateEventsNameTheirAction(t *testing.T) {
	t.Run("an invalid Gate is Validating", func(t *testing.T) {
		g := autoGate("staging", admits("app"))
		g.Spec.Passage.Steps = []v1alpha1.Step{
			{Uses: "git-commit", With: with(`{"mesage":"promote"}`)},
		}
		rec := reconcileWithCapture(t, g, bundleOf(t, "b1", "app"))

		got, ok := rec.find(ReasonInvalidSteps)
		if !ok {
			t.Fatalf("no %s event was recorded; got %+v", ReasonInvalidSteps, rec.events)
		}
		if got.action != "Validating" {
			t.Errorf("action = %q, want %q", got.action, "Validating")
		}
		if got.eventType != corev1.EventTypeWarning {
			t.Errorf("eventType = %q, want Warning", got.eventType)
		}
	})

	t.Run("starting a Passage is Crossing", func(t *testing.T) {
		rec := reconcileWithCapture(t, autoGate("staging", admits("app")), bundleOf(t, "b1", "app"))

		got, ok := rec.find("PassageStarted")
		if !ok {
			t.Fatalf("no PassageStarted event was recorded; got %+v", rec.events)
		}
		if got.action != "Crossing" {
			t.Errorf("action = %q, want %q", got.action, "Crossing")
		}
	})

	t.Run("every event names some action", func(t *testing.T) {
		// The general rule, so an event added later without one is caught even
		// though this test does not know its name.
		rec := reconcileWithCapture(t, autoGate("staging", admits("app")), bundleOf(t, "b1", "app"))
		if len(rec.events) == 0 {
			t.Fatal("no events at all — this test is measuring nothing")
		}
		for _, e := range rec.events {
			if e.action == "" {
				t.Errorf("%s was recorded with no action; the events API requires one", e.reason)
			}
		}
	})
}

// reconcileWithCapture runs one reconcile of the named Gate with a recorder
// that keeps `action`.
func reconcileWithCapture(t *testing.T, g *v1alpha1.Gate, objs ...*v1alpha1.Bundle) *capturing {
	t.Helper()
	builder := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(g).
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{})
	for _, b := range objs {
		builder = builder.WithObjects(b)
	}

	rec := &capturing{}
	r := &Reconciler{
		Client:   builder.Build(),
		Recorder: rec,
		Now:      func() time.Time { return base },
	}
	r.Steps = stepRegistry(t)

	reconcileGate(t, r, g.Name)
	return rec
}

// A nil Recorder must stay survivable: several tests build a Reconciler
// without one, and a controller that panicked when nobody was listening would
// fail a crossing over an event.
func TestGateEventsAreSafeWithoutARecorder(t *testing.T) {
	r := &Reconciler{}
	r.event(&v1alpha1.Gate{}, corev1.EventTypeNormal, "BundleCrossed", "Crossing", "crossed")
}
