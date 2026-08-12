package flux

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// These fixtures are **real output from a real Flux**, captured off a cluster
// rather than written by hand. Hand-written status is status we already believe
// in, so it can only confirm what we already assumed — and every false-green
// this package exists to catch came from an assumption about a field.
//
// They are organised by Flux minor so #91 can add v2.7 and v2.8 alongside
// v2.9 without touching the test: a contract change in a new Flux then shows up
// as a unit-test failure in milliseconds rather than as a support ticket.
const fixtureRoot = "testdata"

// fluxVersions are the directories under testdata. Each holds one file per
// (kind, state) pair captured from that Flux.
func fluxVersions(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		t.Fatal("no captured Flux output at all — the fixtures are the point of this file")
	}
	return versions
}

func loadFixture(t *testing.T, version, name string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, version, name+".json"))
	if err != nil {
		t.Fatalf("fixture %s/%s: %v", version, name, err)
	}
	// apimachinery's json, not encoding/json. The standard decoder turns every
	// number into a float64, and unstructured.NestedInt64 rejects those — so
	// observedGeneration and metadata.generation both read as absent, every
	// resource looks stale, and the whole suite passes or fails for the wrong
	// reason. This decoder does what the API server's does: whole numbers
	// become int64.
	var obj unstructured.Unstructured
	if err := json.Unmarshal(raw, &obj.Object); err != nil {
		t.Fatalf("fixture %s/%s: %v", version, name, err)
	}
	return &obj
}

// The source kinds carry their revision in `status.artifact.revision` rather
// than `lastAppliedRevision`, which is the one place their contract differs
// from a Kustomization's. If that were read wrongly, a flux-wait with an
// expectedRevision against a GitRepository would never match and the crossing
// would hang until its deadline — a failure mode that looks like Flux being
// slow.
func TestSourceKindsAgainstRealFluxOutput(t *testing.T) {
	cases := []struct {
		fixture      string
		wantHealth   v1alpha1.Health
		wantRevision string
	}{
		{"gitrepository-ready", v1alpha1.HealthHealthy,
			"main@sha1:fa90646efc64f7c6e9cba163eddae6ca467f71fa"},
		{"helmrepository-ready", v1alpha1.HealthHealthy,
			"sha256:616e9b4128d1df741234ee73d2411fd56d17c47046c506a7f47b82416c946a9b"},
	}

	for _, version := range fluxVersions(t) {
		for _, tc := range cases {
			t.Run(version+"/"+tc.fixture, func(t *testing.T) {
				got := Evaluate(loadFixture(t, version, tc.fixture), Options{})
				if got.Health != tc.wantHealth {
					t.Errorf("health = %s, want %s (%v)", got.Health, tc.wantHealth, got.Issues)
				}
				if got.Revision != tc.wantRevision {
					t.Errorf("revision = %q, want %q — source kinds report it under "+
						"status.artifact.revision", got.Revision, tc.wantRevision)
				}
			})
		}
	}
}

// Flux retries a failing source for ever, so a bare Ready=False never becomes
// terminal on its own. Reporting Degraded immediately would fail crossings over
// a registry blip; reporting Progressing for ever would hang them. The deadline
// is what separates the two, and these are real failures — a repository that
// does not exist, a registry refusing the connection, a host that does not
// resolve.
func TestFailingSourcesAreProgressingUntilTheDeadline(t *testing.T) {
	for _, version := range fluxVersions(t) {
		for _, fixture := range []string{
			"gitrepository-failing",
			"ocirepository-failing",
			"helmrepository-failing",
		} {
			t.Run(version+"/"+fixture, func(t *testing.T) {
				obj := loadFixture(t, version, fixture)

				// Within the deadline: still trying.
				got := Evaluate(obj, Options{FailAfter: 24 * time.Hour})
				if got.Health != v1alpha1.HealthProgressing {
					t.Errorf("health = %s, want Progressing while Flux is still retrying (%v)",
						got.Health, got.Issues)
				}
				if len(got.Issues) == 0 {
					t.Error("a failing source must say why, or Progressing is indistinguishable from healthy waiting")
				}

				// Past it: give up and say so.
				got = Evaluate(obj, Options{FailAfter: time.Nanosecond})
				if got.Health != v1alpha1.HealthDegraded {
					t.Errorf("health = %s, want Degraded once the failure outlives the deadline",
						got.Health)
				}
			})
		}
	}
}

// A source that has never produced an artifact must not report a revision.
// Returning one would let a flux-wait match against nothing.
func TestFailingSourcesReportNoRevision(t *testing.T) {
	for _, version := range fluxVersions(t) {
		for _, fixture := range []string{
			"gitrepository-failing",
			"ocirepository-failing",
			"helmrepository-failing",
		} {
			t.Run(version+"/"+fixture, func(t *testing.T) {
				if got := Evaluate(loadFixture(t, version, fixture), Options{}); got.Revision != "" {
					t.Errorf("revision = %q, want none from a source that has no artifact", got.Revision)
				}
			})
		}
	}
}

// The false-green this whole package exists for, checked against real output:
// Ready=True describes whatever revision was current when it was set, so a
// status whose observedGeneration lags the spec is describing the *previous*
// spec. A Gate that believed it would admit a Bundle on the strength of the
// source it was pointed at before someone changed the URL.
func TestStaleSourceStatusIsNotHealthy(t *testing.T) {
	for _, version := range fluxVersions(t) {
		t.Run(version, func(t *testing.T) {
			obj := loadFixture(t, version, "gitrepository-ready")
			if got := Evaluate(obj, Options{}); got.Health != v1alpha1.HealthHealthy {
				t.Fatalf("the fixture is not healthy to begin with: %s %v", got.Health, got.Issues)
			}

			// Someone edits the spec. The status has not caught up yet.
			obj.SetGeneration(obj.GetGeneration() + 1)

			if got := Evaluate(obj, Options{}); got.Health != v1alpha1.HealthProgressing {
				t.Errorf("health = %s, want Progressing: this Ready=True describes the "+
					"previous generation", got.Health)
			}
		})
	}
}
