// Package gate decides which Bundles may cross a Gate, and drives the crossing.
package gate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Candidate is one Bundle judged against a Gate.
type Candidate struct {
	Bundle *v1alpha1.Bundle
	// Eligible reports whether this Bundle may cross now.
	Eligible bool
	// Reason explains an ineligible verdict, in words a human can act on.
	// Empty when Eligible.
	Reason string
	// Code is the same verdict as a stable identifier, so a caller can branch
	// without reading English. The approval queue needs to know which Bundles
	// are waiting on a human specifically, and matching on the prose would make
	// rewording a message a breaking change.
	Code Code
}

// Code is a machine-readable reason a Bundle may not cross.
type Code string

const (
	// CodeAlreadyCurrent means it is already in this Gate. Not a rejection.
	CodeAlreadyCurrent Code = "AlreadyCurrent"
	// CodeUpstreamNotCleared means an upstream Gate has not passed it.
	CodeUpstreamNotCleared Code = "UpstreamNotCleared"
	// CodeAwaitingApproval means a human has not approved it for this Gate.
	CodeAwaitingApproval Code = "AwaitingApproval"
)

// Evaluate judges every Bundle against a Gate's admission rules.
//
// It returns a verdict for every Bundle the Gate could conceivably admit —
// including the ineligible ones with their reason — because "why is nothing
// crossing?" is the question operators actually ask, and a filtered list cannot
// answer it.
//
// Bundles from a Beacon this Gate does not admit at all are omitted entirely:
// they are not this Gate's business.
func Evaluate(gate *v1alpha1.Gate, bundles []v1alpha1.Bundle) []Candidate {
	var out []Candidate

	for i := range bundles {
		bundle := &bundles[i]

		admission := admissionFor(gate, bundle)
		if admission == nil {
			continue // not from a Beacon this Gate admits
		}

		eligible, reason, code := judge(gate, admission, bundle)
		out = append(out, Candidate{
			Bundle: bundle, Eligible: eligible, Reason: reason, Code: code,
		})
	}
	return out
}

// admissionFor returns the admission rule covering a Bundle, or nil.
func admissionFor(gate *v1alpha1.Gate, bundle *v1alpha1.Bundle) *v1alpha1.Admission {
	for i := range gate.Spec.Admits {
		if gate.Spec.Admits[i].From.Beacon == bundle.Spec.Beacon {
			return &gate.Spec.Admits[i]
		}
	}
	return nil
}

func judge(
	gate *v1alpha1.Gate, admission *v1alpha1.Admission, bundle *v1alpha1.Bundle,
) (bool, string, Code) {
	// Already here. Not a rejection — there is simply nothing to do.
	if gate.Status.Current != nil && gate.Status.Current.Bundle == bundle.Name {
		return false, "already in this Gate", CodeAlreadyCurrent
	}

	// Upstream clearance. A Gate records a Bundle in status.cleared only once it
	// has crossed *and* passed that Gate's verification, so membership here is
	// the whole check — see D16. Reading the upstream Gate's own status instead
	// would be wrong: that history is capped, so an older Bundle would silently
	// lose its clearance.
	var missing []string
	for _, upstream := range admission.After {
		if !bundle.HasCleared(upstream) {
			missing = append(missing, upstream)
		}
	}
	if len(missing) > 0 {
		return false,
			fmt.Sprintf("has not cleared %s", strings.Join(missing, ", ")),
			CodeUpstreamNotCleared
	}

	if admission.RequireApproval && !bundle.IsApprovedFor(gate.Name) {
		return false, "awaiting approval", CodeAwaitingApproval
	}

	return true, "", ""
}

// Eligible returns just the Bundles that may cross, newest first.
//
// Order matters: callers that pick one want the newest, and callers that
// display the list want the same order the operator would expect.
func Eligible(candidates []Candidate) []*v1alpha1.Bundle {
	var out []*v1alpha1.Bundle
	for _, c := range candidates {
		if c.Eligible {
			out = append(out, c.Bundle)
		}
	}
	sortNewestFirst(out)
	return out
}

// NextAuto returns the Bundle an automatic Gate should cross next, or nil.
//
// Automatic crossings only ever move **forward**: the newest eligible Bundle,
// and only if it is newer than whatever currently occupies the Gate. Without
// that rule a Gate with two eligible Bundles would cross them alternately for
// ever, since neither is "current" once the other arrives.
//
// Rolling back is therefore deliberately not automatic. An older Bundle stays
// eligible and a human can cross it by creating a Passage directly; a
// controller that can roll back on its own is a controller that will.
func NextAuto(candidates []Candidate, current *v1alpha1.Bundle) *v1alpha1.Bundle {
	eligible := Eligible(candidates)
	if len(eligible) == 0 {
		return nil
	}
	newest := eligible[0]

	if current != nil && !newest.CreationTimestamp.After(current.CreationTimestamp.Time) {
		return nil
	}
	return newest
}

// sortNewestFirst orders by creation time, falling back to name so the result
// is stable when timestamps collide — Kubernetes timestamps have one-second
// resolution, and a Beacon can easily emit two Bundles within one second.
func sortNewestFirst(bundles []*v1alpha1.Bundle) {
	slices.SortFunc(bundles, func(a, b *v1alpha1.Bundle) int {
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return strings.Compare(a.Name, b.Name)
		}
		if a.CreationTimestamp.After(b.CreationTimestamp.Time) {
			return -1
		}
		return 1
	})
}
