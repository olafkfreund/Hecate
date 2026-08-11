package ops

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/gate"
)

func TestPromoteOpensAPassage(t *testing.T) {
	o, c := newOps(t, testGate("staging"), testBundle("b1", 0))

	p, err := o.Promote(context.Background(), "acme", "staging", "b1", "olaf@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if p.Spec.Gate != "staging" || p.Spec.Bundle != "b1" {
		t.Errorf("passage = %+v", p.Spec)
	}
	// Who asked is the point of a manual promotion.
	if p.Spec.Actor != "olaf@example.com" {
		t.Errorf("actor = %q", p.Spec.Actor)
	}
	// Built by the controller's own constructor, so the labels other components
	// select on are present.
	if p.Labels[gate.LabelGate] != "staging" || p.Labels[gate.LabelBundle] != "b1" {
		t.Errorf("labels = %v", p.Labels)
	}
	// The steps are copied, so editing the Gate later cannot change what this
	// crossing does.
	if len(p.Spec.Steps) != 1 || p.Spec.Steps[0].Uses != "flux-wait" {
		t.Errorf("steps = %+v", p.Spec.Steps)
	}

	var got v1alpha1.PassageList
	if err := c.List(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Errorf("created %d Passages, want 1", len(got.Items))
	}
}

// A promotion asked for by hand is judged by exactly the rules an automatic one
// is. Skipping them for manual requests is how "promote to production" becomes
// a way around the pipeline.
func TestPromoteAppliesTheSameRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []client.Object
		says string
	}{
		{
			"a Bundle that has not cleared upstream",
			[]client.Object{
				testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Admits[0].After = []string{"dev"} }),
				testBundle("b1", 0),
			},
			"has not cleared dev",
		},
		{
			"a Bundle nobody has approved",
			[]client.Object{
				testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Admits[0].RequireApproval = true }),
				testBundle("b1", 0),
			},
			"awaiting approval",
		},
		{
			"a suspended Gate",
			[]client.Object{
				testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Suspend = true }),
				testBundle("b1", 0),
			},
			"suspended",
		},
		{
			"a Gate with no steps",
			[]client.Object{
				testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Passage = nil }),
				testBundle("b1", 0),
			},
			"no steps",
		},
		{
			"a Bundle from a Beacon this Gate does not admit",
			[]client.Object{
				testGate("staging"),
				testBundle("b1", 0, func(b *v1alpha1.Bundle) { b.Spec.Beacon = "other" }),
			},
			"does not admit",
		},
		{
			"a closed promotion window",
			[]client.Object{
				testGate("staging", func(g *v1alpha1.Gate) {
					g.Spec.Windows = []v1alpha1.Window{
						{Schedule: "0 6 * * 1-5", Duration: metav1.Duration{Duration: time.Hour}},
					}
				}),
				testBundle("b1", 0),
			},
			"window",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, c := newOps(t, tc.objs...)

			_, err := o.Promote(context.Background(), "acme", "staging", "b1", "olaf")
			if !IsRefused(err) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal does not say %q: %v", tc.says, err)
			}

			var got v1alpha1.PassageList
			if err := c.List(context.Background(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != 0 {
				t.Errorf("a refused promotion still created %d Passage(s)", len(got.Items))
			}
		})
	}
}

// Two Passages writing the same environment would race in git, and the loser's
// commit would be silently reverted.
func TestPromoteRefusesWhileOneIsCrossing(t *testing.T) {
	running := passageFor("staging", "b0", v1alpha1.PassageRunning)
	o, _ := newOps(t, testGate("staging"), testBundle("b1", 0), running)

	_, err := o.Promote(context.Background(), "acme", "staging", "b1", "olaf")
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "already crossing") {
		t.Errorf("refusal = %v", err)
	}
}

// An anonymous promotion is worth less than no record, because it looks like
// one.
func TestActionsRequireAnActor(t *testing.T) {
	o, _ := newOps(t, testGate("staging"), testBundle("b1", 0),
		passageFor("staging", "b1", v1alpha1.PassageRunning))
	ctx := context.Background()

	if _, err := o.Promote(ctx, "acme", "staging", "b1", ""); !IsRefused(err) {
		t.Errorf("promote without an actor: %v", err)
	}
	if err := o.Approve(ctx, "acme", "b1", "staging", ""); !IsRefused(err) {
		t.Errorf("approve without an actor: %v", err)
	}
	if err := o.Abort(ctx, "acme", "staging-b1", ""); !IsRefused(err) {
		t.Errorf("abort without an actor: %v", err)
	}
}

func TestApprove(t *testing.T) {
	o, c := newOps(t, testGate("staging"), testGate("production"), testBundle("b1", 0))
	ctx := context.Background()

	if err := o.Approve(ctx, "acme", "b1", "staging", "olaf"); err != nil {
		t.Fatal(err)
	}

	var b v1alpha1.Bundle
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	if !b.IsApprovedFor("staging") {
		t.Error("the approval was not recorded")
	}
	// Per Gate, not per Bundle: approving for staging must not approve for
	// production, which is the whole point of asking.
	if b.IsApprovedFor("production") {
		t.Error("approving for staging also approved for production")
	}

	// Asking twice is not an error, and must not record it twice.
	if err := o.Approve(ctx, "acme", "b1", "staging", "olaf"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "b1"}, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Status.ApprovedFor) != 1 {
		t.Errorf("approvedFor = %v, want one entry", b.Status.ApprovedFor)
	}
}

// Approving for a Gate nobody has defined records an approval that will never
// be read, and reads as done.
func TestApproveRefusesAnUnknownGate(t *testing.T) {
	o, _ := newOps(t, testBundle("b1", 0))

	err := o.Approve(context.Background(), "acme", "b1", "ghost", "olaf")
	if !IsNotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
}

// Approval is what makes an ineligible Bundle eligible, so the two operations
// have to agree.
func TestApprovalUnblocksAPromotion(t *testing.T) {
	g := testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Admits[0].RequireApproval = true })
	o, _ := newOps(t, g, testBundle("b1", 0))
	ctx := context.Background()

	if _, err := o.Promote(ctx, "acme", "staging", "b1", "olaf"); !IsRefused(err) {
		t.Fatalf("expected a refusal before approval, got %v", err)
	}
	if err := o.Approve(ctx, "acme", "b1", "staging", "olaf"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Promote(ctx, "acme", "staging", "b1", "olaf"); err != nil {
		t.Errorf("promotion still refused after approval: %v", err)
	}
}

func TestAbort(t *testing.T) {
	running := passageFor("staging", "b1", v1alpha1.PassageRunning)
	o, c := newOps(t, running)
	ctx := context.Background()

	if err := o.Abort(ctx, "acme", "staging-b1", "olaf"); err != nil {
		t.Fatal(err)
	}

	var p v1alpha1.Passage
	if err := c.Get(ctx, client.ObjectKey{Namespace: "acme", Name: "staging-b1"}, &p); err != nil {
		t.Fatal(err)
	}
	// Set, not deleted: the Passage is the record that a crossing was started
	// and stopped, and deleting it would erase that.
	if !p.Spec.Abort {
		t.Error("abort was not requested")
	}
	if p.Status.Phase == "" {
		t.Error("the Passage was replaced rather than amended")
	}

	// Asking twice is not an error.
	if err := o.Abort(ctx, "acme", "staging-b1", "olaf"); err != nil {
		t.Errorf("aborting twice: %v", err)
	}
}

func TestAbortRefusesAFinishedPassage(t *testing.T) {
	o, _ := newOps(t, passageFor("staging", "b1", v1alpha1.PassageSucceeded))

	err := o.Abort(context.Background(), "acme", "staging-b1", "olaf")
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "already finished") {
		t.Errorf("refusal = %v", err)
	}
}
