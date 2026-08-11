package gate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// DefaultRetain is how many finished Passages a Gate keeps when it does not say.
//
// Higher than the Beacon's, deliberately. A Beacon emits a Bundle whenever an
// artifact appears, so its objects are numerous and individually cheap; a Gate
// produces one Passage per crossing attempt, and each is the record of how
// something got into an environment. Fewer objects, worth more each.
const DefaultRetain int32 = 20

// collect deletes finished Passages beyond the Gate's retention limit, and
// returns how many it removed.
//
// Nothing collected Passages before this, and because a Passage protects its
// Bundle from collection, unbounded Passages meant unbounded Bundles too — so
// Bundle collection (D13) only half-solved growth without it (#108).
//
// The safety rules are the point, and they are deliberately conservative:
//
//   - An unfinished Passage is never collected. Deleting one mid-flight would
//     abandon work the engine is still doing, and leave a Gate waiting on an
//     object that no longer exists.
//   - The Passage that produced the Gate's current occupant is never collected,
//     whatever the limit says. That one is the record of how what is running
//     got there, and a tool whose product is the audit trail must not delete it
//     to save etcd space.
func (r *Reconciler) collect(ctx context.Context, gate *v1alpha1.Gate) (int, error) {
	retain := DefaultRetain
	if gate.Spec.Retain != nil {
		retain = *gate.Spec.Retain
	}
	if retain <= 0 {
		// Zero is "keep everything", not "keep none". Reading it the other way
		// would make an unset-looking field destroy history.
		return 0, nil
	}

	var passages v1alpha1.PassageList
	if err := r.List(ctx, &passages,
		client.InNamespace(gate.Namespace),
		client.MatchingLabels{LabelGate: gate.Name},
	); err != nil {
		return 0, fmt.Errorf("listing Passages: %w", err)
	}

	// Protected by name rather than by being newest: the controller lists from
	// an informer cache that may not have a just-created Passage yet, and a
	// creation timestamp is set by the API server rather than by us. The same
	// reasoning as the Beacon's latestBundle, and for the same reason —
	// collecting what we just created would be a create-delete loop.
	keep := map[string]bool{gate.Status.ActivePassage: true}
	if gate.Status.Current != nil {
		keep[gate.Status.Current.Passage] = true
	}
	// History names the Passage behind each previous occupant. Those are the
	// rollback targets an operator reads when something has gone wrong, so they
	// outrank a retention count.
	for _, occupant := range gate.Status.History {
		keep[occupant.Passage] = true
	}

	var candidates []*v1alpha1.Passage
	for i := range passages.Items {
		p := &passages.Items[i]
		if !p.Status.Phase.Terminal() || keep[p.Name] {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) <= int(retain) {
		return 0, nil
	}

	// Newest first, so the ones kept are the ones someone might still ask about.
	slices.SortFunc(candidates, func(a, b *v1alpha1.Passage) int {
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return strings.Compare(a.Name, b.Name)
		}
		if a.CreationTimestamp.After(b.CreationTimestamp.Time) {
			return -1
		}
		return 1
	})

	var deleted int
	for _, p := range candidates[retain:] {
		if err := r.Delete(ctx, p); err != nil && client.IgnoreNotFound(err) != nil {
			return deleted, fmt.Errorf("deleting Passage %s: %w", p.Name, err)
		}
		deleted++
	}
	return deleted, nil
}
