package api

import (
	"context"
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// The cluster-wide list routes, which is what the UI reads.
//
// **Why these exist.** The screens used to ask for one namespace at a time,
// chosen from a picker, which made "what is happening" a question you could
// only ask about a place you had already guessed. Every page now shows every
// namespace the caller can read, grouped — so these serve the whole answer.
// The namespaced routes remain, for anything asking about one.
//
// **Why not a loop in the browser.** Overview settled this already: one
// cluster-wide List costs the same whether the caller can see one namespace or
// forty, while a request per namespace makes the page slower the more of the
// cluster you are trusted with, which is precisely backwards.
//
// **Why not guard().** guard authorises against a namespace in the path and
// these have none, so it would ask "may you read cluster-wide?" — a right a
// team-scoped operator has no reason to hold, and refusing them a view of their
// own namespaces would be the opposite of the point. Authentication happens
// here; visibility is decided by visibleNamespaces, exactly as overview does.

// visibleOnly keeps the items in namespaces the caller may read, grouped by
// namespace.
//
// Filtering here rather than in Ops is deliberate and matches Overview: Ops
// runs with the server's own credentials and has no idea who is asking, so an
// operations layer that filtered by identity would be a second authorisation
// model to keep in step with this one.
//
// The namespace is taken by function rather than through an interface because
// GetNamespace() is declared on *ObjectMeta, so a []Gate of values would not
// satisfy it while a []*Gate would — a distinction with no meaning here and
// every opportunity to be got wrong later.
func visibleOnly[T any](items []T, namespaces []string, namespaceOf func(T) string) []T {
	allowed := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		allowed[ns] = true
	}

	out := make([]T, 0, len(items))
	for _, it := range items {
		if allowed[namespaceOf(it)] {
			out = append(out, it)
		}
	}
	// Stable, so the order Ops already applied survives inside each namespace
	// and the page does not reshuffle between loads.
	sort.SliceStable(out, func(i, j int) bool {
		return namespaceOf(out[i]) < namespaceOf(out[j])
	})
	return out
}

// listAll runs one cluster-wide read and trims it to what the subject may see.
func listAll[T any](
	ctx context.Context, s *Server, subject Subject,
	read func(context.Context) ([]T, error), namespaceOf func(T) string,
) (any, error) {
	visible, err := s.visibleNamespaces(ctx, subject)
	if err != nil {
		return nil, err
	}
	items, err := read(ctx)
	if err != nil {
		return nil, err
	}
	return visibleOnly(items, visible, namespaceOf), nil
}

func (s *Server) allGates(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	return listAll(ctx, s, subject,
		func(ctx context.Context) ([]v1alpha1.Gate, error) {
			return s.Ops.Gates(ctx, metav1.NamespaceAll)
		},
		func(g v1alpha1.Gate) string { return g.Namespace })
}

func (s *Server) allBundles(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	return listAll(ctx, s, subject,
		func(ctx context.Context) ([]v1alpha1.Bundle, error) {
			return s.Ops.Bundles(ctx, metav1.NamespaceAll)
		},
		func(b v1alpha1.Bundle) string { return b.Namespace })
}

func (s *Server) allBeacons(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	return listAll(ctx, s, subject,
		func(ctx context.Context) ([]v1alpha1.Beacon, error) {
			return s.Ops.Beacons(ctx, metav1.NamespaceAll)
		},
		func(b v1alpha1.Beacon) string { return b.Namespace })
}

func (s *Server) allPassages(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	q := r.URL.Query()
	return listAll(ctx, s, subject,
		func(ctx context.Context) ([]v1alpha1.Passage, error) {
			return s.Ops.Passages(ctx, metav1.NamespaceAll, q.Get("gate"), q.Get("bundle"))
		},
		func(p v1alpha1.Passage) string { return p.Namespace })
}

// allAudit is the one that is not simply a filtered list: Audit reconstructs a
// trail from Gates, Passages and Bundles together, so it is assembled across
// every namespace and then trimmed, rather than assembled per namespace and
// concatenated. Newest-first ordering is the whole point of the page and is
// preserved — unlike the other routes, this one is deliberately not regrouped
// by namespace.
func (s *Server) allAudit(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	visible, err := s.visibleNamespaces(ctx, subject)
	if err != nil {
		return nil, err
	}
	entries, err := s.Ops.Audit(ctx, metav1.NamespaceAll)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(visible))
	for _, ns := range visible {
		allowed[ns] = true
	}
	out := make([]ops.AuditEntry, 0, len(entries))
	for _, e := range entries {
		if allowed[e.Namespace] {
			out = append(out, e)
		}
	}
	return out, nil
}
