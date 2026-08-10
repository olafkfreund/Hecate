package health

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var ksGVK = schema.GroupVersionKind{
	Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
}

// newScheme registers Flux kinds as unstructured so the fake client can serve
// them without importing the Flux controller modules.
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

func cfgJSON(t *testing.T, cfg FluxConfig) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFluxCheckerHealthy(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(kustomization("podinfo", "acme", true, "main@sha1:9f8c1a2b")).
		Build()

	report := NewFluxChecker(cl).Check(context.Background(), Request{
		Namespace: "acme",
		Gate:      "production",
		Config: cfgJSON(t, FluxConfig{
			Resources:        []FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
			ExpectedRevision: "9f8c1a2b",
		}),
	})

	if report.Status != v1alpha1.HealthHealthy {
		t.Fatalf("status = %s, want Healthy (issues: %v)", report.Status, report.Issues)
	}
	if report.Details == nil {
		t.Error("a report should carry per-resource details for the UI")
	}
}

// The Gate's namespace is the default, so `namespace:` is optional.
func TestFluxCheckerDefaultsNamespaceToGate(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(kustomization("podinfo", "team-a", true, "main@sha1:abc")).
		Build()

	report := NewFluxChecker(cl).Check(context.Background(), Request{
		Namespace: "team-a",
		Config:    cfgJSON(t, FluxConfig{Resources: []FluxResource{{Kind: "Kustomization", Name: "podinfo"}}}),
	})
	if report.Status != v1alpha1.HealthHealthy {
		t.Fatalf("status = %s, want Healthy (issues: %v)", report.Status, report.Issues)
	}
}

// A resource we cannot read must surface, never be treated as fine.
func TestFluxCheckerReportsMissingResource(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()

	report := NewFluxChecker(cl).Check(context.Background(), Request{
		Namespace: "acme",
		Config:    cfgJSON(t, FluxConfig{Resources: []FluxResource{{Kind: "Kustomization", Name: "ghost"}}}),
	})
	if report.Status != v1alpha1.HealthUnknown {
		t.Fatalf("status = %s, want Unknown", report.Status)
	}
	if len(report.Issues) == 0 {
		t.Error("a missing resource must produce an issue")
	}
}

func TestFluxCheckerRejectsBadConfig(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()
	checker := NewFluxChecker(cl)

	for name, cfg := range map[string]FluxConfig{
		"no resources": {Resources: nil},
		"no name":      {Resources: []FluxResource{{Kind: "Kustomization"}}},
		"unknown kind": {Resources: []FluxResource{{Kind: "Widget", Name: "x"}}},
		"bad duration": {Resources: []FluxResource{{Kind: "Kustomization", Name: "x"}}, FailAfter: "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			report := checker.Check(context.Background(), Request{Namespace: "acme", Config: cfgJSON(t, cfg)})
			// Unknown, not Degraded: a config error means we never looked at the
			// workload, so claiming it is broken would be a lie.
			if report.Status != v1alpha1.HealthUnknown {
				t.Fatalf("status = %s, want Unknown", report.Status)
			}
		})
	}
}

// The worst resource decides the overall answer.
func TestFluxCheckerMergesAcrossResources(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(
			kustomization("good", "acme", true, "main@sha1:abc"),
			kustomization("bad", "acme", false, ""),
		).
		Build()

	report := NewFluxChecker(cl).Check(context.Background(), Request{
		Namespace: "acme",
		Config: cfgJSON(t, FluxConfig{Resources: []FluxResource{
			{Kind: "Kustomization", Name: "good"},
			{Kind: "Kustomization", Name: "bad"},
		}}),
	})
	if report.Status != v1alpha1.HealthProgressing {
		t.Fatalf("status = %s, want Progressing", report.Status)
	}
}

func TestRegistryAssess(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(ksGVK)).
		WithObjects(kustomization("podinfo", "acme", true, "main@sha1:abc")).
		Build()

	reg := NewRegistry()
	reg.MustRegister(NewFluxChecker(cl))

	t.Run("no checks is not applicable", func(t *testing.T) {
		got := reg.Assess(context.Background(), "acme", "prod", nil)
		if got.Status != v1alpha1.HealthNotApplicable {
			t.Errorf("status = %s, want NotApplicable", got.Status)
		}
	})

	t.Run("registered check runs", func(t *testing.T) {
		got := reg.Assess(context.Background(), "acme", "prod", []v1alpha1.HealthCheck{{
			Uses: CheckerFlux,
			With: &apiextensionsv1.JSON{Raw: cfgJSON(t, FluxConfig{
				Resources: []FluxResource{{Kind: "Kustomization", Name: "podinfo"}},
			})},
		}})
		if got.Status != v1alpha1.HealthHealthy {
			t.Errorf("status = %s, want Healthy (issues: %v)", got.Status, got.Issues)
		}
	})

	t.Run("unknown checker is reported, not skipped", func(t *testing.T) {
		got := reg.Assess(context.Background(), "acme", "prod", []v1alpha1.HealthCheck{{Uses: "nope"}})
		if got.Status != v1alpha1.HealthUnknown {
			t.Errorf("status = %s, want Unknown", got.Status)
		}
		if len(got.Issues) == 0 {
			t.Error("an unregistered checker must be reported; silently skipping it inflates Gate health")
		}
	})
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newScheme(ksGVK)).Build()
	reg := NewRegistry()
	if err := reg.Register(NewFluxChecker(cl)); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := reg.Register(NewFluxChecker(cl)); err == nil {
		t.Error("registering a duplicate name should fail, not silently overwrite")
	}
}
