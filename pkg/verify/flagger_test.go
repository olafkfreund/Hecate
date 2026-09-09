package verify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// crossed is when the crossing under test entered the Gate; canaryRan is when
// Flagger last moved the canary. An ordinary canary is analysed after the
// crossing that triggered it, so fixtures transition after `crossed`.
var (
	crossed   = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	canaryRan = crossed.Add(5 * time.Minute)
)

// canaryObj builds a Canary in the shape Flagger actually writes. The
// conditions block is copied from a real one in the dev cluster: type
// "Promoted", with a reason and a message.
func canaryObj(name, phase string, failed int64, message string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "flagger.app/v1beta1",
		"kind":       "Canary",
		"metadata":   map[string]any{"name": name, "namespace": "acme"},
		"status": map[string]any{
			"phase":              phase,
			"failedChecks":       failed,
			"lastTransitionTime": canaryRan.Format(time.RFC3339),
			"conditions": []any{map[string]any{
				"type": "Promoted", "status": "True", "reason": phase, "message": message,
			}},
		},
	}}
	obj.SetGroupVersionKind(canaryGVK)
	return obj
}

func flagger(objs ...*unstructured.Unstructured) *Flagger {
	b := fake.NewClientBuilder().WithScheme(runtime.NewScheme())
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &Flagger{Client: b.Build()}
}

func verifyCanary(t *testing.T, f *Flagger, name string) (Result, error) {
	t.Helper()
	raw, err := json.Marshal(FlaggerConfig{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return f.Verify(context.Background(), "acme", raw, crossed)
}

func TestASucceededCanaryVerifies(t *testing.T) {
	got, err := verifyCanary(t, flagger(canaryObj("podinfo", "Succeeded", 0, "Promotion completed")), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Verified || !got.Done {
		t.Errorf("result = %+v, want verified and done", got)
	}
}

// The case the feature exists for. A rolled-back canary leaves a healthy
// Deployment serving the previous version, so every health check passes and
// nothing was delivered.
func TestAFailedCanaryDoesNotVerify(t *testing.T) {
	got, err := verifyCanary(t,
		flagger(canaryObj("podinfo", "Failed", 3, "Canary failed! Scaling down podinfo.")), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified {
		t.Fatal("a rolled-back canary verified — the Bundle would clear this Gate and " +
			"be admitted downstream having delivered nothing")
	}
	if !got.Done {
		t.Error("a failed canary is a verdict, not a wait")
	}
	// The count and Flagger's own words: the phase alone does not say which
	// metric tripped.
	for _, want := range []string{"3 failed check", "Scaling down"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not carry %q", got.Reason, want)
		}
	}
}

// "Not finished" must not be reported as failure, or a Gate refuses every
// crossing that has not already completed by the time it looks.
func TestAnUnfinishedCanaryIsNeitherVerifiedNorDone(t *testing.T) {
	for _, phase := range []string{"Initializing", "Waiting", "Progressing", "WaitingPromotion", "Promoting", "Finalising"} {
		t.Run(phase, func(t *testing.T) {
			got, err := verifyCanary(t, flagger(canaryObj("podinfo", phase, 0, "")), "podinfo")
			if err != nil {
				t.Fatal(err)
			}
			if got.Verified {
				t.Errorf("%s verified", phase)
			}
			if got.Done {
				t.Errorf("%s reported as a verdict", phase)
			}
		})
	}
}

// Initialized reads like success and is not: Flagger has set the canary up and
// no analysis has run. Clearing on it would pass a Bundle on the strength of a
// canary that never ran.
func TestInitializedIsNotSuccess(t *testing.T) {
	got, err := verifyCanary(t, flagger(canaryObj("podinfo", "Initialized", 0, "Deployment initialization completed.")), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified {
		t.Error("an initialized canary verified, having run no analysis at all")
	}
}

// A Gate naming a Canary that does not exist is misconfigured. Verifying
// silently would clear Bundles on the strength of a canary nobody is running.
func TestAMissingCanaryIsAnError(t *testing.T) {
	if _, err := verifyCanary(t, flagger(), "nope"); err == nil {
		t.Error("a missing Canary verified")
	}
}

func TestACanaryMustBeNamed(t *testing.T) {
	if _, err := flagger().Verify(context.Background(), "acme", []byte(`{}`), crossed); err == nil {
		t.Error("a verifier with no canary name was accepted")
	}
}

// at overrides when Flagger last moved the canary.
func at(c *unstructured.Unstructured, when time.Time) *unstructured.Unstructured {
	c.Object["status"].(map[string]any)["lastTransitionTime"] = when.Format(time.RFC3339)
	return c
}

// The bug this guards: a Canary keeps its phase after its analysis ends, so one
// that succeeded for the *previous* crossing still reads Succeeded until
// Flagger notices the new pod spec -- an analysis interval later. A Gate
// reconciles the moment its Passage finishes, which is inside that window, so
// reading the phase alone clears a Bundle on the strength of the previous
// crossing's canary. Exactly what the Initialized case above avoids, in the
// phase that occurs far more often.
func TestASucceededCanaryFromBeforeTheCrossingIsNotAVerdict(t *testing.T) {
	stale := at(canaryObj("podinfo", "Succeeded", 0, "Promotion completed"), crossed.Add(-time.Hour))

	got, err := verifyCanary(t, flagger(stale), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified {
		t.Fatal("the previous crossing's canary verified this one -- the Bundle would " +
			"clear this Gate having had no analysis run against it at all")
	}
	if got.Done {
		t.Error("a verdict that predates the crossing is a wait, not an answer: " +
			"Flagger has yet to look at what was just deployed")
	}
}

// The same rule in the other direction, so it cannot be satisfied by refusing
// everything: a canary Flagger analysed after the crossing is this crossing's.
func TestASucceededCanaryFromAfterTheCrossingVerifies(t *testing.T) {
	got, err := verifyCanary(t, flagger(canaryObj("podinfo", "Succeeded", 0, "Promotion completed")), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Verified {
		t.Errorf("result = %+v, want the crossing's own canary to verify it", got)
	}
}

// A failed verdict is dated too. A canary that failed for an earlier crossing
// must not be reported as this crossing's failure, or the Gate says the wrong
// thing about the wrong deployment.
func TestAFailedCanaryFromBeforeTheCrossingIsNotAVerdict(t *testing.T) {
	stale := at(canaryObj("podinfo", "Failed", 3, "Canary failed!"), crossed.Add(-time.Hour))

	got, err := verifyCanary(t, flagger(stale), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified || got.Done {
		t.Errorf("result = %+v, want an earlier crossing's failure treated as a wait", got)
	}
}

// An undated verdict cannot be shown to be this crossing's, and waiting is the
// safe direction: clearing on it is the whole bug above. Flagger always writes
// the field, so this only fires if that ever stops being true.
func TestAnUndatedVerdictDoesNotVerify(t *testing.T) {
	undated := canaryObj("podinfo", "Succeeded", 0, "Promotion completed")
	delete(undated.Object["status"].(map[string]any), "lastTransitionTime")

	got, err := verifyCanary(t, flagger(undated), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified {
		t.Fatal("a verdict with no timestamp verified, so it could have been any crossing's")
	}
	if !strings.Contains(got.Reason, "lastTransitionTime") {
		t.Errorf("reason %q does not say why the verdict could not be dated", got.Reason)
	}
}

// Kubernetes timestamps are second-resolution, so a Canary transitioning in the
// same second the crossing was recorded is reachable rather than theoretical.
// It is not this crossing's verdict either: Flagger's analysis takes at least
// one interval, so a phase set in that same second was set for something else.
func TestAVerdictInTheSameSecondAsTheCrossingIsNotAVerdict(t *testing.T) {
	same := at(canaryObj("podinfo", "Succeeded", 0, "Promotion completed"), crossed)

	got, err := verifyCanary(t, flagger(same), "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified {
		t.Error("a canary that moved in the same second the crossing was recorded verified it, " +
			"having had no interval in which to analyse anything")
	}
}
