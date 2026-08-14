package verify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
			"phase":        phase,
			"failedChecks": failed,
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
	return f.Verify(context.Background(), "acme", raw)
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
	if _, err := flagger().Verify(context.Background(), "acme", []byte(`{}`)); err == nil {
		t.Error("a verifier with no canary name was accepted")
	}
}
