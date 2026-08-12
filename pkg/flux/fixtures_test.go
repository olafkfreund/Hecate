package flux

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
// Each kind reports its revision somewhere different, and *where* is the
// contract — a Flux release moving one is exactly the change that would
// otherwise surface as a flux-wait hanging until its deadline on a resource
// that converged ages ago.
//
// The expected value is read back out of each fixture rather than written as a
// literal, so a version captured from a different commit needs no test change.
var revisionField = map[string][]string{
	"gitrepository-ready":  {"status", "artifact", "revision"},
	"helmrepository-ready": {"status", "artifact", "revision"},
	"kustomization-ready":  {"status", "lastAppliedRevision"},
	// A HelmRelease has no lastAppliedRevision at all. It carries the chart
	// version in *two* places — lastAttemptedRevision and
	// history[0].chartVersion — which for a Ready release always agree.
	//
	// So unlike the others, this entry pins a **value, not a path**: removing
	// either field from appliedRevision's chain leaves the other answering
	// correctly, and no assertion on the result can tell them apart. The
	// ordering is pinned by the hand-written TestAppliedRevisionFromHelmRelease*
	// tests instead, and that the two agree is asserted just below.
	//
	// Worth writing down because the first version of this table claimed to
	// test the history path and quietly did not. Mutation testing found that;
	// reading the fixture did not.
	"helmrelease-ready": {"status", "lastAttemptedRevision"},
}

// The history fallback in appliedRevision is not reached by any real Ready
// HelmRelease, because lastAttemptedRevision comes first in the chain and is
// always set by the time one is Ready. It is kept for a release that has not
// got that far, and the hand-built TestAppliedRevisionFromHelmReleaseHistory
// covers it — but this records that the two agree, which is what makes the
// ordering safe rather than merely untested.
func TestHelmReleaseHistoryAgreesWithTheAttemptedRevision(t *testing.T) {
	for _, version := range fluxVersions(t) {
		t.Run(version, func(t *testing.T) {
			obj := loadFixture(t, version, "helmrelease-ready")
			attempted := nested(t, obj, []string{"status", "lastAttemptedRevision"})
			history := nested(t, obj, []string{"status", "history", "0", "chartVersion"})
			if attempted == "" || history == "" {
				t.Fatalf("attempted=%q history=%q — recapture the fixture", attempted, history)
			}
			if attempted != history {
				t.Errorf("lastAttemptedRevision %q and history[0].chartVersion %q disagree: "+
					"appliedRevision prefers the first, so which one is right now matters",
					attempted, history)
			}
		})
	}
}

func TestReadyResourcesReportTheRightRevision(t *testing.T) {
	for _, version := range fluxVersions(t) {
		for fixture, path := range revisionField {
			t.Run(version+"/"+fixture, func(t *testing.T) {
				obj := loadFixture(t, version, fixture)
				got := Evaluate(obj, Options{})

				if got.Health != v1alpha1.HealthHealthy {
					t.Errorf("health = %s, want Healthy (%v)", got.Health, got.Issues)
				}

				want := nested(t, obj, path)
				if want == "" {
					t.Fatalf("the fixture has nothing at %v; recapture it", path)
				}
				if got.Revision != want {
					t.Errorf("revision = %q, want %q from %v", got.Revision, want, path)
				}
			})
		}
	}
}

// nested reads a string out of an unstructured object, treating a numeric
// element as a slice index so `status.history.0.chartVersion` can be written
// as a path like any other.
func nested(t *testing.T, obj *unstructured.Unstructured, path []string) string {
	t.Helper()
	var cur any = obj.Object
	for _, step := range path {
		if i, err := strconv.Atoi(step); err == nil {
			list, ok := cur.([]any)
			if !ok || i >= len(list) {
				return ""
			}
			cur = list[i]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[step]
	}
	s, _ := cur.(string)
	return s
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

// failingFixtures are the captured resources that are genuinely broken — a
// repository that does not exist, a registry refusing connections, a host that
// does not resolve, a kustomize path that is not there, a chart that was never
// published.
//
// Workload kinds are in here as well as sources, and that matters: they carry
// the same `observedGeneration: -1` sentinel when they have never reconciled,
// so D45 is a rule about Flux rather than a quirk of GitRepository. Nothing
// proved that until these fixtures existed.
var failingFixtures = []string{
	"gitrepository-failing",
	"ocirepository-failing",
	"helmrepository-failing",
	"kustomization-failing",
	"helmrelease-failing",
}

// Flux retries a failing source for ever, so a bare Ready=False never becomes
// terminal on its own. Reporting Degraded immediately would fail crossings over
// a registry blip; reporting Progressing for ever would hang them. The deadline
// is what separates the two, and these are real failures — a repository that
// does not exist, a registry refusing the connection, a host that does not
// resolve.
func TestFailingSourcesAreProgressingUntilTheDeadline(t *testing.T) {
	for _, version := range fluxVersions(t) {
		for _, fixture := range failingFixtures {
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
		for _, fixture := range failingFixtures {
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
