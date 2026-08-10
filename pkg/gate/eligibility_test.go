package gate

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var base = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func bundle(name, beacon string, ageMinutes int, cleared ...string) v1alpha1.Bundle {
	b := v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "acme",
			CreationTimestamp: metav1.Time{Time: base.Add(time.Duration(ageMinutes) * time.Minute)},
		},
		Spec: v1alpha1.BundleSpec{Beacon: beacon},
	}
	for _, g := range cleared {
		b.Status.Cleared = append(b.Status.Cleared, v1alpha1.GateCrossing{Gate: g})
	}
	return b
}

func gateAdmitting(name string, admissions ...v1alpha1.Admission) *v1alpha1.Gate {
	return &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec:       v1alpha1.GateSpec{Admits: admissions},
	}
}

func admits(beacon string, after ...string) v1alpha1.Admission {
	return v1alpha1.Admission{From: v1alpha1.BundleOrigin{Beacon: beacon}, After: after}
}

func find(t *testing.T, cs []Candidate, name string) Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Bundle.Name == name {
			return c
		}
	}
	t.Fatalf("no candidate named %q in %d results", name, len(cs))
	return Candidate{}
}

// The heart of the issue: admitting a Bundle that has not cleared upstream
// silently defeats the whole pipeline.
func TestUpstreamClearanceIsRequired(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo", "staging"))

	cs := Evaluate(g, []v1alpha1.Bundle{
		bundle("uncleared", "podinfo", 0),
		bundle("cleared", "podinfo", 1, "staging"),
	})

	if c := find(t, cs, "uncleared"); c.Eligible {
		t.Error("a Bundle that has not cleared staging must not be eligible for production")
	} else if c.Reason != "has not cleared staging" {
		t.Errorf("reason = %q, want it to name the missing Gate", c.Reason)
	}

	if c := find(t, cs, "cleared"); !c.Eligible {
		t.Errorf("a Bundle that cleared staging should be eligible, got %q", c.Reason)
	}
}

func TestEveryUpstreamGateMustBeCleared(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo", "staging", "canary"))

	cs := Evaluate(g, []v1alpha1.Bundle{
		bundle("partial", "podinfo", 0, "staging"),
		bundle("full", "podinfo", 1, "staging", "canary"),
	})

	if c := find(t, cs, "partial"); c.Eligible {
		t.Error("clearing one of two upstream Gates must not be enough")
	} else if c.Reason != "has not cleared canary" {
		t.Errorf("reason = %q, want it to name only the missing Gate", c.Reason)
	}
	if !find(t, cs, "full").Eligible {
		t.Error("clearing both upstream Gates should be eligible")
	}
}

// An entry Gate has no upstream, so anything from its Beacon is admissible.
func TestEntryGateNeedsNoClearance(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	cs := Evaluate(g, []v1alpha1.Bundle{bundle("fresh", "podinfo", 0)})
	if !find(t, cs, "fresh").Eligible {
		t.Error("a Gate with no `after` should admit straight from the Beacon")
	}
}

func TestApprovalIsRequiredWhenAsked(t *testing.T) {
	a := admits("podinfo", "staging")
	a.RequireApproval = true
	g := gateAdmitting("production", a)

	unapproved := bundle("unapproved", "podinfo", 0, "staging")
	approved := bundle("approved", "podinfo", 1, "staging")
	approved.Status.ApprovedFor = []string{"production"}
	elsewhere := bundle("elsewhere", "podinfo", 2, "staging")
	elsewhere.Status.ApprovedFor = []string{"some-other-gate"}

	cs := Evaluate(g, []v1alpha1.Bundle{unapproved, approved, elsewhere})

	if c := find(t, cs, "unapproved"); c.Eligible {
		t.Error("approval is required but was not given")
	} else if c.Reason != "awaiting approval" {
		t.Errorf("reason = %q, want 'awaiting approval'", c.Reason)
	}
	if !find(t, cs, "approved").Eligible {
		t.Error("an approved Bundle should be eligible")
	}
	// Approval is per Gate. Approving for staging must not unlock production.
	if find(t, cs, "elsewhere").Eligible {
		t.Error("approval for a different Gate must not count")
	}
}

func TestBundlesFromOtherBeaconsAreIgnored(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo"))
	cs := Evaluate(g, []v1alpha1.Bundle{
		bundle("ours", "podinfo", 0),
		bundle("theirs", "some-other-service", 0),
	})
	if len(cs) != 1 || cs[0].Bundle.Name != "ours" {
		t.Errorf("a Gate should only judge Bundles from Beacons it admits, got %d candidates", len(cs))
	}
}

func TestCurrentOccupantIsNotEligible(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	g.Status.Current = &v1alpha1.GateOccupant{Bundle: "here-already"}

	cs := Evaluate(g, []v1alpha1.Bundle{bundle("here-already", "podinfo", 0)})
	if c := find(t, cs, "here-already"); c.Eligible {
		t.Error("the Bundle already in the Gate has nothing to do")
	} else if c.Reason != "already in this Gate" {
		t.Errorf("reason = %q", c.Reason)
	}
}

// Having crossed before must not disqualify a Bundle, or rollback becomes
// impossible for ever.
func TestPreviouslyClearedBundleStaysEligible(t *testing.T) {
	g := gateAdmitting("production", admits("podinfo", "staging"))
	old := bundle("v1", "podinfo", 0, "staging", "production")

	cs := Evaluate(g, []v1alpha1.Bundle{old})
	if !find(t, cs, "v1").Eligible {
		t.Error("a Bundle that previously crossed must remain eligible so it can be rolled back to")
	}
}

func TestEligibleIsNewestFirst(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	cs := Evaluate(g, []v1alpha1.Bundle{
		bundle("oldest", "podinfo", 0),
		bundle("newest", "podinfo", 10),
		bundle("middle", "podinfo", 5),
	})

	got := Eligible(cs)
	want := []string{"newest", "middle", "oldest"}
	if len(got) != 3 {
		t.Fatalf("got %d eligible, want 3", len(got))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, name)
		}
	}
}

// Kubernetes timestamps have one-second resolution, so ties are routine.
func TestEligibleOrderIsStableOnTies(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	cs := Evaluate(g, []v1alpha1.Bundle{
		bundle("b", "podinfo", 0),
		bundle("a", "podinfo", 0),
	})
	first := Eligible(cs)[0].Name
	for i := 0; i < 5; i++ {
		if got := Eligible(cs)[0].Name; got != first {
			t.Fatalf("order is unstable: %q then %q", first, got)
		}
	}
	if first != "a" {
		t.Errorf("ties should break by name, got %q first", first)
	}
}

func TestNextAutoOnlyMovesForward(t *testing.T) {
	g := gateAdmitting("dev", admits("podinfo"))
	older := bundle("older", "podinfo", 0)
	newer := bundle("newer", "podinfo", 10)

	t.Run("empty Gate takes the newest", func(t *testing.T) {
		cs := Evaluate(g, []v1alpha1.Bundle{older, newer})
		got := NextAuto(cs, nil)
		if got == nil || got.Name != "newer" {
			t.Fatalf("got %v, want newer", got)
		}
	})

	t.Run("refuses to go backwards", func(t *testing.T) {
		// This is the flip-flop guard: with `newer` in place, `older` is still
		// eligible, and an unguarded controller would cross it right back.
		occupied := gateAdmitting("dev", admits("podinfo"))
		occupied.Status.Current = &v1alpha1.GateOccupant{Bundle: "newer"}
		cs := Evaluate(occupied, []v1alpha1.Bundle{older, newer})

		if got := NextAuto(cs, &newer); got != nil {
			t.Errorf("auto crossed backwards to %q", got.Name)
		}
	})

	t.Run("nothing eligible", func(t *testing.T) {
		blocked := gateAdmitting("production", admits("podinfo", "staging"))
		cs := Evaluate(blocked, []v1alpha1.Bundle{bundle("nope", "podinfo", 0)})
		if got := NextAuto(cs, nil); got != nil {
			t.Errorf("got %q, want nothing", got.Name)
		}
	})
}
