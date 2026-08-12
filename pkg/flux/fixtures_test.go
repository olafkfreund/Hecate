package flux

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
	for _, version := range fluxVersions(t) {
		for _, fixture := range []string{"gitrepository-ready", "helmrepository-ready"} {
			t.Run(version+"/"+fixture, func(t *testing.T) {
				obj := loadFixture(t, version, fixture)
				got := Evaluate(obj, Options{})

				if got.Health != v1alpha1.HealthHealthy {
					t.Errorf("health = %s, want Healthy (%v)", got.Health, got.Issues)
				}

				// Compared against the fixture's own artifact revision rather
				// than a literal, so a version whose commit differs needs no
				// test change — and so this asserts the thing that matters,
				// which is *which field* a source's revision comes from.
				want, found, err := unstructured.NestedString(obj.Object, "status", "artifact", "revision")
				if err != nil || !found || want == "" {
					t.Fatalf("the fixture has no status.artifact.revision; recapture it")
				}
				if got.Revision != want {
					t.Errorf("revision = %q, want %q — source kinds report it under "+
						"status.artifact.revision, not lastAppliedRevision", got.Revision, want)
				}
			})
		}
	}
}

// Every version must offer the same fixtures, or a kind quietly stops being
// covered on one Flux and the suite still passes.
func TestEveryFluxVersionHasTheSameFixtures(t *testing.T) {
	versions := fluxVersions(t)
	if len(versions) < 2 {
		t.Skip("only one version captured; nothing to compare")
	}

	names := func(v string) []string {
		entries, err := os.ReadDir(filepath.Join(fixtureRoot, v))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				out = append(out, e.Name())
			}
		}
		sort.Strings(out)
		return out
	}

	want := names(versions[0])
	for _, v := range versions[1:] {
		if got := names(v); !slices.Equal(got, want) {
			t.Errorf("%s has %v but %s has %v — every version must cover the same kinds",
				v, got, versions[0], want)
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
