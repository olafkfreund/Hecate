// Package flux evaluates the health of Flux CD resources (Kustomization,
// HelmRelease, and anything else following the Flux status conventions).
//
// It reads resources as unstructured objects and inspects the documented
// status contract:
//
//   - metadata.generation vs status.observedGeneration
//   - status.conditions[type=Ready|Stalled]
//   - status.lastAppliedRevision / lastAttemptedRevision
//   - spec.suspend
//
// ponytail: unstructured instead of importing fluxcd/kustomize-controller/api
// and helm-controller/api. Avoids four module dependencies that would pin us
// to one Flux minor version, and the contract above is stable across v1/v2.
// Switch to typed APIs only if we need fields the contract doesn't expose.
package flux

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// DefaultFailAfter is how long a resource may sit un-Ready before we stop
// calling it Progressing and call it Degraded.
//
// Flux retries most failures forever, so "Ready=False" on its own never becomes
// terminal. Without a deadline a broken deploy reports Progressing indefinitely,
// which reads as "still working" when it means "wedged". Callers should tune
// this to their slowest legitimate rollout.
const DefaultFailAfter = 10 * time.Minute

// Result is the evaluation of a single resource.
type Result struct {
	// Health is the assessment.
	Health v1alpha1.Health
	// Revision is the revision Flux reports as applied, if any.
	Revision string
	// Issues explain any non-Healthy result, in human-readable form.
	Issues []string
}

// Options tune the evaluation.
type Options struct {
	// ExpectedRevision, when non-empty, requires the resource to have applied
	// this revision before it is considered Healthy. A bare git SHA matches a
	// Flux revision string such as "main@sha1:9f8c...": see RevisionMatches.
	ExpectedRevision string
	// FailAfter is how long a resource may be un-Ready before it is reported
	// Degraded rather than Progressing. Zero means DefaultFailAfter.
	FailAfter time.Duration
	// Now is the clock, injectable for tests. Zero means time.Now().
	Now time.Time
}

func (o Options) failAfter() time.Duration {
	if o.FailAfter <= 0 {
		return DefaultFailAfter
	}
	return o.FailAfter
}

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// Evaluate determines the health of a single Flux resource.
func Evaluate(obj *unstructured.Unstructured, opts Options) Result {
	if obj == nil {
		return Result{Health: v1alpha1.HealthUnknown, Issues: []string{"resource not found"}}
	}

	ref := describe(obj)

	if suspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend"); suspended {
		// A suspended resource will never converge on its own. Reporting
		// Progressing here would hang a Passage forever waiting on a human.
		return Result{
			Health: v1alpha1.HealthUnknown,
			Issues: []string{fmt.Sprintf("%s is suspended; Flux will not reconcile it", ref)},
		}
	}

	revision := appliedRevision(obj)

	// A stale status is the classic false-green: spec has moved on, Ready=True
	// still describes the previous generation. Check this before conditions.
	gen := obj.GetGeneration()
	observed, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if !found || observed < gen {
		return Result{
			Health:   v1alpha1.HealthProgressing,
			Revision: revision,
			Issues: []string{fmt.Sprintf(
				"%s has not observed generation %d yet (observedGeneration=%d)", ref, gen, observed,
			)},
		}
	}

	// Stalled=True is Flux saying explicitly that it has stopped retrying.
	if c, ok := condition(obj, "Stalled"); ok && c.Status == "True" {
		return Result{
			Health:   v1alpha1.HealthDegraded,
			Revision: revision,
			Issues:   []string{fmt.Sprintf("%s is stalled: %s: %s", ref, c.Reason, c.Message)},
		}
	}

	ready, ok := condition(obj, "Ready")
	if !ok {
		return Result{
			Health:   v1alpha1.HealthProgressing,
			Revision: revision,
			Issues:   []string{fmt.Sprintf("%s has no Ready condition yet", ref)},
		}
	}

	switch ready.Status {
	case "True":
		if opts.ExpectedRevision != "" && !RevisionMatches(revision, opts.ExpectedRevision) {
			return Result{
				Health:   v1alpha1.HealthProgressing,
				Revision: revision,
				Issues: []string{fmt.Sprintf(
					"%s is Ready at revision %q, waiting for %q", ref, revision, opts.ExpectedRevision,
				)},
			}
		}
		return Result{Health: v1alpha1.HealthHealthy, Revision: revision}

	case "False":
		issue := fmt.Sprintf("%s is not ready: %s: %s", ref, ready.Reason, ready.Message)
		// Flux keeps retrying, so treat a recent failure as Progressing and only
		// call it Degraded once it has been failing longer than the deadline.
		if !ready.LastTransitionTime.IsZero() &&
			opts.now().Sub(ready.LastTransitionTime) > opts.failAfter() {
			return Result{
				Health:   v1alpha1.HealthDegraded,
				Revision: revision,
				Issues: []string{fmt.Sprintf(
					"%s (not ready for %s)", issue, opts.now().Sub(ready.LastTransitionTime).Round(time.Second),
				)},
			}
		}
		return Result{Health: v1alpha1.HealthProgressing, Revision: revision, Issues: []string{issue}}

	default: // "Unknown" — Flux is mid-reconcile
		return Result{
			Health:   v1alpha1.HealthProgressing,
			Revision: revision,
			Issues:   []string{fmt.Sprintf("%s readiness is unknown: %s", ref, ready.Message)},
		}
	}
}

// RevisionMatches reports whether a Flux revision string satisfies an expected
// revision.
//
// Flux revisions carry the source ref alongside the digest, in one of several
// shapes across versions and source kinds:
//
//	main@sha1:9f8c1a2b3c4d...   (GitRepository, Flux >= 0.36)
//	main/9f8c1a2b3c4d...        (GitRepository, older)
//	sha256:abcd...              (OCIRepository)
//	6.3.5                       (HelmChart version)
//
// The expected value is usually a bare git SHA produced by an upstream step, and
// may be abbreviated. We therefore compare the digest portion with prefix
// tolerance in whichever direction is shorter.
func RevisionMatches(actual, expected string) bool {
	a, e := digestOf(actual), digestOf(expected)
	if a == "" || e == "" {
		return false
	}
	if len(a) < len(e) {
		a, e = e, a
	}
	return strings.HasPrefix(a, e)
}

// digestOf strips the source-ref prefix from a Flux revision string.
func digestOf(rev string) string {
	rev = strings.TrimSpace(rev)
	// "main@sha1:9f8c..." / "sha256:abcd..." -> the part after the last ':'
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	// "main/9f8c..." -> the part after the last '/'
	if i := strings.LastIndex(rev, "/"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

// appliedRevision returns the revision Flux reports for this resource, trying
// the fields used by Kustomization, HelmRelease and the source kinds in turn.
func appliedRevision(obj *unstructured.Unstructured) string {
	for _, path := range [][]string{
		{"status", "lastAppliedRevision"},
		{"status", "lastAttemptedRevision"},
		{"status", "artifact", "revision"},
	} {
		if v, found, _ := unstructured.NestedString(obj.Object, path...); found && v != "" {
			return v
		}
	}
	// HelmRelease v2 records its applied chart under status.history[0].
	if hist, found, _ := unstructured.NestedSlice(obj.Object, "status", "history"); found && len(hist) > 0 {
		if entry, ok := hist[0].(map[string]any); ok {
			for _, key := range []string{"chartVersion", "digest"} {
				if v, ok := entry[key].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// cond is a status condition, decoded from unstructured.
type cond struct {
	Status             string
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

func condition(obj *unstructured.Unstructured, condType string) (cond, bool) {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return cond{}, false
	}
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != condType {
			continue
		}
		c := cond{}
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		if ts, ok := m["lastTransitionTime"].(string); ok {
			c.LastTransitionTime, _ = time.Parse(time.RFC3339, ts)
		}
		return c, true
	}
	return cond{}, false
}

func describe(obj *unstructured.Unstructured) string {
	ns := obj.GetNamespace()
	if ns == "" {
		return fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	}
	return fmt.Sprintf("%s %s/%s", obj.GetKind(), ns, obj.GetName())
}
