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

func explain(t *testing.T, objs ...client.Object) *Explanation {
	t.Helper()
	o, _ := newOps(t, objs...)
	ex, err := o.Explain(context.Background(), "acme", "staging")
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// has reports whether the explanation carries a blocker of this kind.
func has(ex *Explanation, kind BlockerKind) *Blocker {
	for i := range ex.Blockers {
		if ex.Blockers[i].Kind == kind {
			return &ex.Blockers[i]
		}
	}
	return nil
}

// The question the whole operation exists for, in the shape an operator asks
// it: nothing is crossing, and the answer must say what to do about it.
func TestExplainNamesWhatToFix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		state   State
		blocker BlockerKind
		says    string
		fix     string
	}{
		{
			name:  "a suspended Gate",
			objs:  []client.Object{testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Suspend = true })},
			state: StateBlocked, blocker: BlockerSuspended, says: "suspended", fix: "suspend",
		},
		{
			name:  "a Gate with no steps",
			objs:  []client.Object{testGate("staging", func(g *v1alpha1.Gate) { g.Spec.Passage = nil })},
			state: StateBlocked, blocker: BlockerNoPassage, says: "no steps",
		},
		{
			name:  "a Gate with nothing to promote",
			objs:  []client.Object{testGate("staging")},
			state: StateIdle, blocker: BlockerNoBundles, says: "no Bundle",
		},
		{
			name: "a Bundle that has not cleared upstream",
			objs: []client.Object{
				testGate("staging", func(g *v1alpha1.Gate) {
					g.Spec.Admits[0].After = []string{"dev"}
				}),
				testBundle("b1", 0),
			},
			state: StateBlocked, blocker: BlockerUpstream, says: "has not cleared dev",
			fix: "upstream",
		},
		{
			name: "a Bundle awaiting approval",
			objs: []client.Object{
				testGate("staging", func(g *v1alpha1.Gate) {
					g.Spec.Admits[0].RequireApproval = true
				}),
				testBundle("b1", 0),
			},
			state: StateBlocked, blocker: BlockerNotApproved, says: "awaiting approval",
			fix: "hecate approve b1",
		},
		{
			name:  "a manual Gate with something ready",
			objs:  []client.Object{testGate("staging"), testBundle("b1", 0)},
			state: StateReady, blocker: BlockerManual, says: "ready to cross",
			fix: "hecate promote staging --bundle b1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := explain(t, tc.objs...)

			if ex.State != tc.state {
				t.Errorf("state = %s, want %s (%s)", ex.State, tc.state, ex.Summary)
			}
			b := has(ex, tc.blocker)
			if b == nil {
				t.Fatalf("no %s blocker; got %+v", tc.blocker, ex.Blockers)
			}
			if !strings.Contains(ex.Summary+" "+b.Detail, tc.says) {
				t.Errorf("nothing says %q: %s / %s", tc.says, ex.Summary, b.Detail)
			}
			// A blocker without a fix leaves the operator where they started.
			if tc.fix != "" && !strings.Contains(b.Fix, tc.fix) {
				t.Errorf("fix = %q, want it to mention %q", b.Fix, tc.fix)
			}
		})
	}
}

// A crossing in flight is the answer by itself, and the step's own message is
// the specific thing being waited on — a held change gate, a pull request.
func TestExplainAnInFlightCrossing(t *testing.T) {
	p := passageFor("staging", "b1", v1alpha1.PassageRunning)
	p.Status.Steps = []v1alpha1.StepStatus{
		{Uses: "git-push", Phase: v1alpha1.StepSucceeded},
		{Uses: "evidence-gate", Phase: v1alpha1.StepRunning,
			Message: "change gate is holding (risk 20): awaiting a human approval"},
	}
	ex := explain(t, testGate("staging"), testBundle("b1", 0), p)

	if ex.State != StateCrossing {
		t.Fatalf("state = %s, want Crossing", ex.State)
	}
	for _, want := range []string{"evidence-gate", "awaiting a human approval"} {
		if !strings.Contains(ex.Summary, want) {
			t.Errorf("summary does not say %q: %s", want, ex.Summary)
		}
	}
	if has(ex, BlockerStepWaiting) == nil {
		t.Error("no StepWaiting blocker")
	}
}

// A failed crossing is the first thing an operator needs, and the step's reason
// code is what makes it classifiable rather than merely readable (D21).
func TestExplainAFailedCrossing(t *testing.T) {
	p := passageFor("staging", "b1", v1alpha1.PassageFailed)
	p.Status.Message = "git-push: authentication failed"
	p.Status.Steps = []v1alpha1.StepStatus{
		{Uses: "git-clone", Phase: v1alpha1.StepSucceeded},
		{Uses: "git-push", Phase: v1alpha1.StepFailed,
			Reason: "GitAuthFailed", Message: "the host rejected our credentials"},
	}
	ex := explain(t, testGate("staging"), testBundle("b1", 0), p)

	if ex.State != StateFailed {
		t.Fatalf("state = %s, want Failed (%s)", ex.State, ex.Summary)
	}
	b := has(ex, BlockerPassageFailed)
	if b == nil {
		t.Fatal("no PassageFailed blocker")
	}
	for _, want := range []string{"git-push", "GitAuthFailed", "rejected our credentials"} {
		if !strings.Contains(b.Detail, want) {
			t.Errorf("detail does not say %q: %s", want, b.Detail)
		}
	}
}

// A Passage that failed before a later one succeeded is history. Reporting it
// would make a working Gate look broken.
func TestAnOlderFailureIsNotABlocker(t *testing.T) {
	old := passageFor("staging", "b0", v1alpha1.PassageFailed)
	old.Name = "staging-old"
	old.CreationTimestamp = metav1.Time{Time: base.Add(-time.Hour)}

	recent := passageFor("staging", "b1", v1alpha1.PassageSucceeded)
	recent.Name = "staging-recent"

	ex := explain(t, testGate("staging"), testBundle("b1", 0), old, recent)

	if b := has(ex, BlockerPassageFailed); b != nil {
		t.Errorf("an old failure was reported as a blocker: %s", b.Detail)
	}
	if ex.State == StateFailed {
		t.Errorf("state = Failed, but the most recent crossing succeeded")
	}
}

// A closed window is why something eligible is not moving, and it is otherwise
// invisible: nothing in the Gate's status says "not yet".
func TestExplainAClosedWindow(t *testing.T) {
	g := testGate("staging", func(g *v1alpha1.Gate) {
		g.Spec.Auto = true
		// Weekday mornings only; base is 12:00 UTC.
		g.Spec.Windows = []v1alpha1.Window{{Schedule: "0 6 * * 1-5", Duration: metav1.Duration{Duration: time.Hour}}}
	})
	ex := explain(t, g, testBundle("b1", 0))

	if ex.State != StateBlocked {
		t.Fatalf("state = %s, want Blocked (%s)", ex.State, ex.Summary)
	}
	if has(ex, BlockerWindowClosed) == nil {
		t.Errorf("no WindowClosed blocker: %+v", ex.Blockers)
	}
	// It is still eligible — the window is a delay, not a rejection.
	if len(ex.Eligible) != 1 {
		t.Errorf("eligible = %v, want the Bundle to remain eligible", ex.Eligible)
	}
}

// Degraded health is not why nothing crosses, but it is nearly always what the
// operator actually wanted to know.
func TestExplainReportsHealth(t *testing.T) {
	g := testGate("staging", func(g *v1alpha1.Gate) {
		g.Status.Health = &v1alpha1.HealthReport{
			Status: v1alpha1.HealthDegraded,
			Issues: []string{"Kustomization podinfo: Ready=False"},
		}
	})
	ex := explain(t, g)

	if ex.Health != v1alpha1.HealthDegraded {
		t.Errorf("health = %s", ex.Health)
	}
	b := has(ex, BlockerUnhealthy)
	if b == nil {
		t.Fatal("no Unhealthy blocker")
	}
	if !strings.Contains(b.Detail, "Kustomization podinfo") {
		t.Errorf("detail = %s", b.Detail)
	}
}

// A Gate the controller refused to run must say so, and say what is wrong —
// not report that nothing is eligible, which would be a true fact and a
// misleading answer.
func TestExplainAnInvalidGate(t *testing.T) {
	g := testGate("staging", func(g *v1alpha1.Gate) {
		g.Status.Conditions = []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason:             gate.ReasonInvalidSteps,
			Message:            `steps[1] (git-commit): invalid configuration: json: unknown field "mesage"`,
			LastTransitionTime: metav1.Time{Time: base},
		}}
	})
	ex := explain(t, g, testBundle("b1", 0))

	if ex.State != StateBlocked {
		t.Fatalf("state = %s, want Blocked", ex.State)
	}
	b := has(ex, BlockerInvalidSteps)
	if b == nil {
		t.Fatal("no InvalidSteps blocker")
	}
	if !strings.Contains(b.Detail, "mesage") {
		t.Errorf("the detail does not carry the controller's message: %s", b.Detail)
	}
}

// The Bundle already in the Gate is the goal, not a problem.
func TestTheCurrentBundleIsNotWaiting(t *testing.T) {
	g := testGate("staging", func(g *v1alpha1.Gate) {
		g.Status.Current = &v1alpha1.GateOccupant{Bundle: "b1"}
	})
	ex := explain(t, g, testBundle("b1", 0))

	if ex.Current != "b1" {
		t.Errorf("current = %q", ex.Current)
	}
	for _, w := range ex.Waiting {
		if w.Bundle == "b1" {
			t.Errorf("the Bundle already in the Gate was reported as waiting: %s", w.Reason)
		}
	}
	if ex.State != StateIdle {
		t.Errorf("state = %s, want Idle", ex.State)
	}
}
