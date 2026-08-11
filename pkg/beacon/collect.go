package beacon

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// DefaultRetain is how many unreferenced Bundles survive when a Beacon does not
// say. Enough to roll back a few releases by hand; not enough to fill etcd.
const DefaultRetain int32 = 10

// collect deletes unreferenced Bundles beyond the Beacon's retention limit, and
// returns how many it removed.
//
// The safety rule is the whole point, and it is deliberately conservative: a
// Bundle in use is never collected, whatever the limit says. Deleting the record
// of what is running in production to save etcd space would be a spectacular own
// goal for a tool whose product is the audit trail. See D13.
func (r *Reconciler) collect(ctx context.Context, beacon *v1alpha1.Beacon) (int, error) {
	retain := DefaultRetain
	if beacon.Spec.Retain != nil {
		retain = *beacon.Spec.Retain
	}
	if retain <= 0 {
		// Zero is "keep everything", not "keep none". Reading it the other way
		// would make an unset-looking field destroy history.
		return 0, nil
	}

	var bundles v1alpha1.BundleList
	if err := r.List(ctx, &bundles,
		client.InNamespace(beacon.Namespace),
		client.MatchingLabels{LabelBeacon: beacon.Name},
	); err != nil {
		return 0, fmt.Errorf("listing Bundles: %w", err)
	}

	inUse, err := r.bundlesInUse(ctx, beacon.Namespace)
	if err != nil {
		return 0, err
	}

	// The Bundle this Beacon most recently emitted is protected by name rather
	// than by being newest. Ordering alone is too weak a guarantee: the
	// controller's List reads an informer cache that may not have the new
	// Bundle yet, and a creation timestamp is set by the API server, not by us.
	// Collecting what we just emitted would be an infinite discover-delete loop.
	if beacon.Status.LatestBundle != "" {
		inUse[beacon.Status.LatestBundle] = true
	}

	// Only unreferenced Bundles are candidates, newest first, so the ones we
	// keep are the ones someone might still want.
	var candidates []*v1alpha1.Bundle
	for i := range bundles.Items {
		b := &bundles.Items[i]
		if inUse[b.Name] || len(b.Status.ApprovedFor) > 0 {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) <= int(retain) {
		return 0, nil
	}

	slices.SortFunc(candidates, func(a, b *v1alpha1.Bundle) int {
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return strings.Compare(a.Name, b.Name)
		}
		if a.CreationTimestamp.After(b.CreationTimestamp.Time) {
			return -1
		}
		return 1
	})

	var deleted int
	for _, b := range candidates[retain:] {
		if err := r.Delete(ctx, b); err != nil && client.IgnoreNotFound(err) != nil {
			return deleted, fmt.Errorf("deleting Bundle %s: %w", b.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

// bundlesInUse names every Bundle that must survive collection.
//
// Referenced by a Passage covers more than it first appears: every Bundle that
// ever crossed a Gate has one, so in practice collection reaps the Bundles that
// were discovered and never promoted — which is exactly the noise a Beacon on a
// short interval generates.
func (r *Reconciler) bundlesInUse(ctx context.Context, namespace string) (map[string]bool, error) {
	inUse := map[string]bool{}

	var gates v1alpha1.GateList
	if err := r.List(ctx, &gates, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Gates: %w", err)
	}
	for i := range gates.Items {
		if current := gates.Items[i].Status.Current; current != nil {
			inUse[current.Bundle] = true
		}
	}

	var passages v1alpha1.PassageList
	if err := r.List(ctx, &passages, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Passages: %w", err)
	}
	for i := range passages.Items {
		inUse[passages.Items[i].Spec.Bundle] = true
	}

	return inUse, nil
}
