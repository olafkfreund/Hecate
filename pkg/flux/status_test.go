package flux

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

var now = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// ks builds a Kustomization-shaped unstructured object.
func ks(gen, observed int64, mutate func(m map[string]any)) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]any{
			"name":       "podinfo",
			"namespace":  "flux-system",
			"generation": gen,
		},
		"spec": map[string]any{},
		"status": map[string]any{
			"observedGeneration": observed,
		},
	}
	if mutate != nil {
		mutate(obj)
	}
	return &unstructured.Unstructured{Object: obj}
}

func status(obj map[string]any) map[string]any { return obj["status"].(map[string]any) }

func readyCond(s, reason, msg string, at time.Time) []any {
	return []any{map[string]any{
		"type": "Ready", "status": s, "reason": reason, "message": msg,
		"lastTransitionTime": at.Format(time.RFC3339),
	}}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		opts Options
		want v1alpha1.Health
	}{
		{
			name: "ready and current is healthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:9f8c1a2b3c4d5e6f"
			}),
			opts: Options{Now: now},
			want: v1alpha1.HealthHealthy,
		},
		{
			name: "ready at the expected revision is healthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:9f8c1a2b3c4d5e6f"
			}),
			opts: Options{Now: now, ExpectedRevision: "9f8c1a2b"},
			want: v1alpha1.HealthHealthy,
		},
		{
			name: "ready at the wrong revision is still progressing",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:aaaaaaaaaaaa"
			}),
			opts: Options{Now: now, ExpectedRevision: "9f8c1a2b"},
			want: v1alpha1.HealthProgressing,
		},
		{
			// The false-green case: Ready=True describes the previous generation.
			name: "stale observedGeneration is progressing even when ready",
			obj: ks(2, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
			}),
			opts: Options{Now: now},
			want: v1alpha1.HealthProgressing,
		},
		{
			name: "recent failure is progressing because Flux retries",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("False", "BuildFailed", "kustomize build failed", now.Add(-2*time.Minute))
			}),
			opts: Options{Now: now, FailAfter: 10 * time.Minute},
			want: v1alpha1.HealthProgressing,
		},
		{
			name: "failure past the deadline is unhealthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("False", "BuildFailed", "kustomize build failed", now.Add(-30*time.Minute))
			}),
			opts: Options{Now: now, FailAfter: 10 * time.Minute},
			want: v1alpha1.HealthDegraded,
		},
		{
			name: "stalled is unhealthy immediately",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = append(
					readyCond("False", "RetriesExhausted", "upgrade retries exhausted", now),
					map[string]any{"type": "Stalled", "status": "True", "reason": "RetriesExhausted", "message": "giving up"},
				)
			}),
			opts: Options{Now: now},
			want: v1alpha1.HealthDegraded,
		},
		{
			name: "suspended is unknown, not progressing",
			obj: ks(1, 1, func(m map[string]any) {
				m["spec"].(map[string]any)["suspend"] = true
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
			}),
			opts: Options{Now: now},
			want: v1alpha1.HealthUnknown,
		},
		{
			name: "no conditions yet is progressing",
			obj:  ks(1, 1, nil),
			opts: Options{Now: now},
			want: v1alpha1.HealthProgressing,
		},
		{
			name: "missing resource is unknown",
			obj:  nil,
			opts: Options{Now: now},
			want: v1alpha1.HealthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.obj, tt.opts)
			if got.Health != tt.want {
				t.Fatalf("state = %q, want %q (issues: %v)", got.Health, tt.want, got.Issues)
			}
			if tt.want != v1alpha1.HealthHealthy && len(got.Issues) == 0 {
				t.Errorf("non-healthy result must explain itself, got no issues")
			}
		})
	}
}

func TestRevisionMatches(t *testing.T) {
	tests := []struct {
		actual, expected string
		want             bool
	}{
		{"main@sha1:9f8c1a2b3c4d", "9f8c1a2b3c4d", true},
		{"main@sha1:9f8c1a2b3c4d", "9f8c1a2b", true},  // abbreviated expectation
		{"9f8c1a2b", "main@sha1:9f8c1a2b3c4d", true},  // reversed, still matches
		{"main/9f8c1a2b3c4d", "9f8c1a2b3c4d", true},   // pre-0.36 format
		{"sha256:abcdef123456", "abcdef123456", true}, // OCIRepository digest
		{"6.3.5", "6.3.5", true},                      // HelmChart version
		{"main@sha1:9f8c1a2b3c4d", "aaaaaaaa", false},
		{"main@sha1:9f8c1a2b3c4d", "", false},
		{"", "9f8c1a2b", false},
	}
	for _, tt := range tests {
		if got := RevisionMatches(tt.actual, tt.expected); got != tt.want {
			t.Errorf("RevisionMatches(%q, %q) = %v, want %v", tt.actual, tt.expected, got, tt.want)
		}
	}
}

// HelmRelease records its revision in a different place than Kustomization.
func TestAppliedRevisionFromHelmReleaseHistory(t *testing.T) {
	hr := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "HelmRelease",
		"metadata": map[string]any{"name": "podinfo", "namespace": "default", "generation": int64(1)},
		"spec":     map[string]any{},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"conditions":         readyCond("True", "InstallSucceeded", "installed", now),
			"history":            []any{map[string]any{"chartVersion": "6.3.5"}},
		},
	}}
	got := Evaluate(hr, Options{Now: now, ExpectedRevision: "6.3.5"})
	if got.Health != v1alpha1.HealthHealthy {
		t.Fatalf("state = %q, want Healthy (issues: %v)", got.Health, got.Issues)
	}
	if got.Revision != "6.3.5" {
		t.Errorf("revision = %q, want 6.3.5", got.Revision)
	}
}

// Flux writes status.observedGeneration = -1 on a resource that has never
// successfully reconciled, while its conditions already describe the current
// generation. Believing only the top-level field reports a permanently broken
// source as "not observed yet" for ever: never Degraded however long it fails,
// and with the real error hidden behind a message about generations.
//
// This is stated separately from the captured fixtures because it is the rule,
// not an example of it — a future Flux could stop using -1 and this must still
// hold for any status whose conditions are current.
func TestNeverReconciledIsJudgedByItsConditions(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"name": "fleet", "namespace": "acme", "generation": int64(1)},
		"status": map[string]any{
			"observedGeneration": int64(-1),
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "False",
					"observedGeneration": int64(1),
					"reason":             "GitOperationFailed",
					"message":            "repository not found",
					"lastTransitionTime": "2026-08-12T11:53:50Z",
				},
			},
		},
	}}

	// Long enough that Flux is still plausibly retrying.
	got := Evaluate(obj, Options{
		Now:       time.Date(2026, 8, 12, 11, 54, 0, 0, time.UTC),
		FailAfter: time.Hour,
	})
	if got.Health != v1alpha1.HealthProgressing {
		t.Errorf("health = %s, want Progressing while the failure is fresh", got.Health)
	}
	if !strings.Contains(got.Issues[0], "repository not found") {
		t.Errorf("issue = %q, want the actual error rather than a message about generations",
			got.Issues[0])
	}

	// And it must eventually give up, which is impossible if the staleness
	// check short-circuits on the -1.
	got = Evaluate(obj, Options{
		Now:       time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC),
		FailAfter: time.Minute,
	})
	if got.Health != v1alpha1.HealthDegraded {
		t.Errorf("health = %s, want Degraded: a source that has failed for an hour "+
			"is not still starting up", got.Health)
	}
}

// The genuine stale case must still be caught: a condition left over from a
// previous spec carries that spec's generation, and must not be believed.
func TestConditionFromAPreviousGenerationIsStale(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": "app", "namespace": "acme", "generation": int64(4)},
		"status": map[string]any{
			"observedGeneration":  int64(3),
			"lastAppliedRevision": "main@sha1:abc",
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "True",
					"observedGeneration": int64(3),
					"reason":             "ReconciliationSucceeded",
					"lastTransitionTime": "2026-08-12T11:53:50Z",
				},
			},
		},
	}}

	if got := Evaluate(obj, Options{}); got.Health != v1alpha1.HealthProgressing {
		t.Errorf("health = %s, want Progressing: this Ready=True describes generation 3, "+
			"not the current 4", got.Health)
	}
}
