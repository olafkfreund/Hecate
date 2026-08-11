package gate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// stubStep is a registered step with a config of a known shape, so these tests
// do not depend on any real step's fields.
type stubStep struct{ name string }

func (s stubStep) Name() string { return s.name }

func (s stubStep) Run(context.Context, *passage.StepContext) (passage.StepResult, error) {
	return passage.StepResult{Phase: v1alpha1.StepSucceeded}, nil
}

func (s stubStep) CheckConfig(raw json.RawMessage) error {
	_, err := passage.CheckConfig[struct {
		Message string `json:"message"`
	}](raw)
	return err
}

func stepRegistry(t *testing.T) *passage.Registry {
	t.Helper()
	r := passage.NewRegistry()
	r.MustRegister(stubStep{name: "flux-wait"})
	r.MustRegister(stubStep{name: "git-commit"})
	return r
}

func with(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

// bundleOf is the eligibility helper's Bundle, as a pointer for the fake client.
func bundleOf(t *testing.T, name, beacon string) *v1alpha1.Bundle {
	t.Helper()
	b := bundle(name, beacon, 0)
	return &b
}

// A Gate whose steps will not run must say so immediately and cross nothing.
// The alternative is finding out part-way through a production crossing, with a
// commit already pushed and half a Passage to unpick.
func TestAGateWithBadStepsRefusesToCross(t *testing.T) {
	g := autoGate("staging", admits("app"))
	g.Spec.Passage.Steps = []v1alpha1.Step{
		{Uses: "git-commit", With: with(`{"mesage":"promote"}`)},
	}
	bundle := bundleOf(t, "b1", "app")

	r, c, rec := newReconciler(t, g, bundle)
	r.Steps = stepRegistry(t)

	reconcileGate(t, r, "staging")

	got := getGate(t, c, "staging")
	cond := ready(t, got)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", cond)
	}
	if cond.Reason != ReasonInvalidSteps {
		t.Errorf("reason = %s, want %s", cond.Reason, ReasonInvalidSteps)
	}
	// The message has to name the step and the field, or the author is left
	// looking at a Gate that reads correctly.
	for _, want := range []string{"steps[0]", "git-commit", "mesage"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message does not mention %q: %s", want, cond.Message)
		}
	}

	// Nothing eligible, and nothing started: an invalid Gate is inert.
	if len(got.Status.Eligible) != 0 {
		t.Errorf("eligible = %v, want none", got.Status.Eligible)
	}
	var passages v1alpha1.PassageList
	if err := c.List(context.Background(), &passages); err != nil {
		t.Fatal(err)
	}
	if len(passages.Items) != 0 {
		t.Errorf("an invalid Gate opened %d Passage(s)", len(passages.Items))
	}

	// And it is visible without reading status by hand.
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, ReasonInvalidSteps) {
			t.Errorf("event = %q", e)
		}
	default:
		t.Error("no event was recorded for an invalid Gate")
	}
}

// Fixing the Gate must clear it: a controller that latched the error would make
// the fix invisible.
func TestFixingTheStepsClearsTheRefusal(t *testing.T) {
	g := autoGate("staging", admits("app"))
	g.Spec.Passage.Steps = []v1alpha1.Step{{Uses: "git-commit", With: with(`{"mesage":"x"}`)}}

	r, c, _ := newReconciler(t, g, bundleOf(t, "b1", "app"))
	r.Steps = stepRegistry(t)
	reconcileGate(t, r, "staging")

	if cond := ready(t, getGate(t, c, "staging")); cond.Reason != ReasonInvalidSteps {
		t.Fatalf("expected the Gate to be refused first, got %s", cond.Reason)
	}

	fixed := getGate(t, c, "staging")
	fixed.Spec.Passage.Steps = []v1alpha1.Step{{Uses: "git-commit", With: with(`{"message":"x"}`)}}
	if err := c.Update(context.Background(), fixed); err != nil {
		t.Fatal(err)
	}
	reconcileGate(t, r, "staging")

	if cond := ready(t, getGate(t, c, "staging")); cond.Reason == ReasonInvalidSteps {
		t.Errorf("the Gate is still refused after being fixed: %s", cond.Message)
	}
}

// A controller with no registry wired must not decide every Gate is broken.
func TestAGateIsNotJudgedWithoutARegistry(t *testing.T) {
	g := autoGate("staging", admits("app"))
	g.Spec.Passage.Steps = []v1alpha1.Step{{Uses: "nonexistent"}}

	r, c, _ := newReconciler(t, g, bundleOf(t, "b1", "app"))
	r.Steps = nil

	reconcileGate(t, r, "staging")

	if cond := ready(t, getGate(t, c, "staging")); cond.Reason == ReasonInvalidSteps {
		t.Error("a Gate was refused by a controller that has no step registry")
	}
}

// A suspended Gate is not running anything, so its steps are nobody's problem
// yet — and reporting Suspended is more useful than reporting both.
func TestSuspensionIsReportedBeforeStepProblems(t *testing.T) {
	g := autoGate("staging", admits("app"))
	g.Spec.Suspend = true
	g.Spec.Passage.Steps = []v1alpha1.Step{{Uses: "nonexistent"}}

	r, c, _ := newReconciler(t, g)
	r.Steps = stepRegistry(t)

	reconcileGate(t, r, "staging")

	if cond := ready(t, getGate(t, c, "staging")); cond.Reason != "Suspended" {
		t.Errorf("reason = %s, want Suspended", cond.Reason)
	}
}
