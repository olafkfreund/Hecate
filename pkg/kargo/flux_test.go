package kargo

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/pkg/health"
	"github.com/akuity/kargo/pkg/promotion"
)

// newScheme registers the Flux kinds as unstructured so the fake client can
// serve them without importing the Flux controller modules.
func newScheme(gvks ...schema.GroupVersionKind) *runtime.Scheme {
	s := runtime.NewScheme()
	for _, gvk := range gvks {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return s
}

func kustomization(name, ns string, ready bool, revision string) *unstructured.Unstructured {
	st := "True"
	reason := "ReconciliationSucceeded"
	if !ready {
		st = "False"
		reason = "BuildFailed"
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

var ksGVK = schema.GroupVersionKind{
	Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
}

func TestCheckerReportsHealthy(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(kustomization("podinfo", "my-project", true, "main@sha1:9f8c1a2b")).
		Build()

	res := NewChecker(cl).Check(context.Background(), "my-project", "prod", health.Criteria{
		Kind: CheckerKindFlux,
		Input: health.Input{
			"resources":        []any{map[string]any{"kind": "Kustomization", "name": "podinfo"}},
			"expectedRevision": "9f8c1a2b",
		},
	})

	if res.Status != kargoapi.HealthStateHealthy {
		t.Fatalf("status = %q, want Healthy (issues: %v)", res.Status, res.Issues)
	}
}

// The Stage's project is the default namespace, so `namespace:` is optional.
func TestCheckerDefaultsNamespaceToProject(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(kustomization("podinfo", "team-a", true, "main@sha1:abc")).
		Build()

	res := NewChecker(cl).Check(context.Background(), "team-a", "prod", health.Criteria{
		Input: health.Input{"resources": []any{map[string]any{"kind": "Kustomization", "name": "podinfo"}}},
	})
	if res.Status != kargoapi.HealthStateHealthy {
		t.Fatalf("status = %q, want Healthy (issues: %v)", res.Status, res.Issues)
	}
}

// A missing resource must surface, not be silently treated as fine.
func TestCheckerReportsMissingResource(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()

	res := NewChecker(cl).Check(context.Background(), "my-project", "prod", health.Criteria{
		Input: health.Input{"resources": []any{map[string]any{"kind": "Kustomization", "name": "ghost"}}},
	})
	if res.Status != kargoapi.HealthStateUnknown {
		t.Fatalf("status = %q, want Unknown", res.Status)
	}
	if len(res.Issues) == 0 {
		t.Error("a missing resource must produce an issue")
	}
}

func TestCheckerRejectsBadConfig(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()
	checker := NewChecker(cl)

	for name, input := range map[string]health.Input{
		"no resources": {"resources": []any{}},
		"no name":      {"resources": []any{map[string]any{"kind": "Kustomization"}}},
		"unknown kind": {"resources": []any{map[string]any{"kind": "Widget", "name": "x"}}},
		"bad duration": {
			"resources": []any{map[string]any{"kind": "Kustomization", "name": "x"}},
			"failAfter": "not-a-duration",
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := checker.Check(context.Background(), "p", "s", health.Criteria{Input: input})
			if res.Status != kargoapi.HealthStateUnknown {
				t.Fatalf("status = %q, want Unknown", res.Status)
			}
		})
	}
}

func TestFluxWaitRunsUntilReady(t *testing.T) {
	ctx := context.Background()

	t.Run("not ready yet keeps running", func(t *testing.T) {
		cl := fake.NewClientBuilder().
			WithScheme(newScheme(ksGVK)).
			WithObjects(kustomization("podinfo", "p", false, "")).
			Build()
		runner := newFluxWaiter(promotion.StepRunnerCapabilities{KargoClient: cl})

		res, err := runner.Run(ctx, &promotion.StepContext{
			Project: "p",
			Config: promotion.Config{
				"resources": []any{map[string]any{"kind": "Kustomization", "name": "podinfo"}},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != kargoapi.PromotionStepStatusRunning {
			t.Fatalf("status = %q, want Running", res.Status)
		}
		if res.RetryAfter == nil {
			t.Error("a Running step should suggest a retry interval")
		}
	})

	t.Run("ready succeeds and emits the health check", func(t *testing.T) {
		cl := fake.NewClientBuilder().
			WithScheme(newScheme(ksGVK)).
			WithObjects(kustomization("podinfo", "p", true, "main@sha1:9f8c1a2b")).
			Build()
		runner := newFluxWaiter(promotion.StepRunnerCapabilities{KargoClient: cl})

		res, err := runner.Run(ctx, &promotion.StepContext{
			Project: "p",
			Config: promotion.Config{
				"resources":        []any{map[string]any{"kind": "Kustomization", "name": "podinfo"}},
				"expectedRevision": "9f8c1a2b",
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != kargoapi.PromotionStepStatusSucceeded {
			t.Fatalf("status = %q, want Succeeded (%s)", res.Status, res.Message)
		}
		// The step must hand the Stage a health check watching the same
		// resources, otherwise the Stage goes blind after the promotion.
		if res.HealthCheck == nil {
			t.Fatal("step must return health check criteria")
		}
		if res.HealthCheck.Kind != CheckerKindFlux {
			t.Errorf("health check kind = %q, want %q", res.HealthCheck.Kind, CheckerKindFlux)
		}
		if res.HealthCheck.Input["expectedRevision"] != "9f8c1a2b" {
			t.Errorf("health check lost expectedRevision: %v", res.HealthCheck.Input)
		}
	})

	t.Run("bad config is terminal, not retried forever", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()
		runner := newFluxWaiter(promotion.StepRunnerCapabilities{KargoClient: cl})

		_, err := runner.Run(ctx, &promotion.StepContext{
			Project: "p",
			Config:  promotion.Config{"resources": []any{}},
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !promotion.IsTerminal(err) {
			t.Errorf("bad config must be a TerminalError, got %T: %v", err, err)
		}
	})
}

// Register must be callable and must actually install both plugin points.
func TestRegister(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()
	Register(cl)

	if _, err := promotion.DefaultStepRunnerRegistry.Get(StepKindFluxWait); err != nil {
		t.Errorf("flux-wait not registered: %v", err)
	}
}
