package steps

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

var ksGVK = schema.GroupVersionKind{
	Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(ksGVK, &unstructured.Unstructured{})
	listGVK := ksGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func kustomization(name, ns string, ready bool, revision string) *unstructured.Unstructured {
	st, reason := "True", "ReconciliationSucceeded"
	if !ready {
		st, reason = "False", "BuildFailed"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": name, "namespace": ns, "generation": int64(1)},
		"spec":       map[string]any{},
		"status": map[string]any{
			"observedGeneration":  int64(1),
			"lastAppliedRevision": revision,
			"conditions": []any{map[string]any{
				"type": "Ready", "status": st, "reason": reason, "message": "msg",
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}},
		},
	}}
}

func stepCtx(t *testing.T, cfg health.FluxConfig) *passage.StepContext {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &passage.StepContext{Namespace: "acme", Gate: "production", Config: raw}
}

func TestFluxWaitKeepsRunningUntilReady(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(kustomization("podinfo", "acme", false, "")).
		Build()
	step := NewFluxWait(health.NewFluxChecker(cl))

	res, err := step.Run(context.Background(), stepCtx(t, health.FluxConfig{
		Resources: []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != v1alpha1.StepRunning {
		t.Fatalf("phase = %s, want Running", res.Phase)
	}
	if res.RetryAfter <= 0 {
		t.Error("a waiting step must suggest when to try again")
	}
}

func TestFluxWaitSucceedsAndHandsTheGateItsWatch(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(kustomization("podinfo", "acme", true, "main@sha1:9f8c1a2b")).
		Build()
	step := NewFluxWait(health.NewFluxChecker(cl))

	res, err := step.Run(context.Background(), stepCtx(t, health.FluxConfig{
		Resources:        []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
		ExpectedRevision: "9f8c1a2b",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("phase = %s, want Succeeded (%s)", res.Phase, res.Message)
	}

	// Without this the Gate stops watching the moment the Passage ends.
	if len(res.Watch) != 1 {
		t.Fatalf("step must emit exactly one health check, got %d", len(res.Watch))
	}
	if res.Watch[0].Uses != health.CheckerFlux {
		t.Errorf("watch uses %q, want %q", res.Watch[0].Uses, health.CheckerFlux)
	}
	var handed health.FluxConfig
	if err := json.Unmarshal(res.Watch[0].With.Raw, &handed); err != nil {
		t.Fatalf("emitted watch config is not decodable: %v", err)
	}
	if handed.ExpectedRevision != "9f8c1a2b" {
		t.Errorf("the emitted watch lost expectedRevision: %+v", handed)
	}
}

// A revision the Passage did not push must not satisfy the wait.
func TestFluxWaitWaitsForTheRightRevision(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(kustomization("podinfo", "acme", true, "main@sha1:oldoldold")).
		Build()
	step := NewFluxWait(health.NewFluxChecker(cl))

	res, err := step.Run(context.Background(), stepCtx(t, health.FluxConfig{
		Resources:        []health.FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
		ExpectedRevision: "9f8c1a2b",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Phase != v1alpha1.StepRunning {
		t.Fatalf("phase = %s, want Running: Ready at the wrong revision is not done", res.Phase)
	}
}

func TestFluxWaitBadConfigIsTerminal(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	step := NewFluxWait(health.NewFluxChecker(cl))

	_, err := step.Run(context.Background(), stepCtx(t, health.FluxConfig{Resources: nil}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !passage.IsTerminal(err) {
		t.Errorf("bad config must be terminal, got %T: %v", err, err)
	}
}
