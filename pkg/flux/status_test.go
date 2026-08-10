package flux

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		want State
	}{
		{
			name: "ready and current is healthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:9f8c1a2b3c4d5e6f"
			}),
			opts: Options{Now: now},
			want: StateHealthy,
		},
		{
			name: "ready at the expected revision is healthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:9f8c1a2b3c4d5e6f"
			}),
			opts: Options{Now: now, ExpectedRevision: "9f8c1a2b"},
			want: StateHealthy,
		},
		{
			name: "ready at the wrong revision is still progressing",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
				status(m)["lastAppliedRevision"] = "main@sha1:aaaaaaaaaaaa"
			}),
			opts: Options{Now: now, ExpectedRevision: "9f8c1a2b"},
			want: StateProgressing,
		},
		{
			// The false-green case: Ready=True describes the previous generation.
			name: "stale observedGeneration is progressing even when ready",
			obj: ks(2, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
			}),
			opts: Options{Now: now},
			want: StateProgressing,
		},
		{
			name: "recent failure is progressing because Flux retries",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("False", "BuildFailed", "kustomize build failed", now.Add(-2*time.Minute))
			}),
			opts: Options{Now: now, FailAfter: 10 * time.Minute},
			want: StateProgressing,
		},
		{
			name: "failure past the deadline is unhealthy",
			obj: ks(1, 1, func(m map[string]any) {
				status(m)["conditions"] = readyCond("False", "BuildFailed", "kustomize build failed", now.Add(-30*time.Minute))
			}),
			opts: Options{Now: now, FailAfter: 10 * time.Minute},
			want: StateUnhealthy,
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
			want: StateUnhealthy,
		},
		{
			name: "suspended is unknown, not progressing",
			obj: ks(1, 1, func(m map[string]any) {
				m["spec"].(map[string]any)["suspend"] = true
				status(m)["conditions"] = readyCond("True", "ReconciliationSucceeded", "applied", now)
			}),
			opts: Options{Now: now},
			want: StateUnknown,
		},
		{
			name: "no conditions yet is progressing",
			obj:  ks(1, 1, nil),
			opts: Options{Now: now},
			want: StateProgressing,
		},
		{
			name: "missing resource is unknown",
			obj:  nil,
			opts: Options{Now: now},
			want: StateUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.obj, tt.opts)
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (issues: %v)", got.State, tt.want, got.Issues)
			}
			if tt.want != StateHealthy && len(got.Issues) == 0 {
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

func TestStateMerge(t *testing.T) {
	// A set of resources is only as healthy as its worst member.
	tests := []struct {
		a, b, want State
	}{
		{StateHealthy, StateHealthy, StateHealthy},
		{StateHealthy, StateProgressing, StateProgressing},
		{StateProgressing, StateUnhealthy, StateUnhealthy},
		{StateUnknown, StateProgressing, StateUnknown},
		{StateUnhealthy, StateUnknown, StateUnhealthy},
	}
	for _, tt := range tests {
		if got := tt.a.Merge(tt.b); got != tt.want {
			t.Errorf("%s.Merge(%s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
		if got := tt.b.Merge(tt.a); got != tt.want {
			t.Errorf("Merge must be commutative: %s.Merge(%s) = %s, want %s", tt.b, tt.a, got, tt.want)
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
	if got.State != StateHealthy {
		t.Fatalf("state = %q, want Healthy (issues: %v)", got.State, got.Issues)
	}
	if got.Revision != "6.3.5" {
		t.Errorf("revision = %q, want 6.3.5", got.Revision)
	}
}
