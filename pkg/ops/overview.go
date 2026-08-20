package ops

import (
	"context"
	"fmt"
	"sort"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Overview is every Gate the caller can see, and what is true of them.
//
// The shape answers "is everything okay?" before it answers anything else,
// which is the question someone opening a dashboard is actually asking. The
// per-Gate detail is there for the follow-up, and the pages that already exist
// are there for the one after that.
type Overview struct {
	// Namespaces holds one entry per namespace with Gates in it, ordered by
	// name so the page does not reshuffle between loads.
	Namespaces []NamespaceOverview `json:"namespaces"`
	// Totals is the whole picture in numbers.
	Totals Totals `json:"totals"`
}

// NamespaceOverview is one namespace's Gates.
type NamespaceOverview struct {
	Namespace string        `json:"namespace"`
	Gates     []GateSummary `json:"gates"`
}

// GateSummary is one Gate, reduced to what a board shows.
type GateSummary struct {
	Name string `json:"name"`
	// Health is the Gate's own report, or Unknown when it has none yet.
	Health v1alpha1.Health `json:"health"`
	// Issues is why the health is what it is. Carried here rather than left to
	// a click, because a board that shows a red dot and no reason sends
	// everyone to the same second page to find out.
	Issues []string `json:"issues,omitempty"`
	// Current is the Bundle in the Gate now.
	Current string `json:"current,omitempty"`
	// Eligible counts what could cross. The names are one page away; the count
	// is what decides whether to look.
	Eligible int `json:"eligible"`
	// Running is the Passage crossing this Gate now, if one is.
	Running string `json:"running,omitempty"`
	// Suspended means the Gate will not admit anything until it is resumed.
	// Reported prominently for the same reason a suspended Beacon is: a
	// suspended Gate looks exactly like a quiet one, and "nothing has shipped
	// all week" is usually this.
	Suspended bool `json:"suspended"`
}

// Totals counts what matters across everything visible.
type Totals struct {
	Gates       int `json:"gates"`
	Healthy     int `json:"healthy"`
	Progressing int `json:"progressing"`
	Degraded    int `json:"degraded"`
	Unknown     int `json:"unknown"`
	Suspended   int `json:"suspended"`
	// Eligible is how many Bundles could cross somewhere right now.
	Eligible int `json:"eligible"`
	// Running is how many Passages are in flight.
	Running int `json:"running"`
	// Failed is how many Passages ended badly and are still around to say so.
	// Retention collects Passages (D40), so this is "recently" by definition
	// rather than by a window this code picks.
	Failed int `json:"failed"`
}

// Overview assembles the board for the namespaces the caller may read.
//
// The namespaces are passed in rather than discovered here: this runs with the
// server's own credentials and has no idea who is asking, so deciding what is
// visible is the API layer's job, where the subject is known. Same division as
// Namespaces, and for the same reason — an operations layer that filtered by
// identity would be a second authorisation model.
//
// Two cluster-wide Lists rather than a pair per namespace: the work is the same
// whether one namespace is visible or forty, and a loop of Lists would make the
// cost of the page scale with how much of the cluster you are trusted with —
// which is precisely backwards.
func (o *Ops) Overview(ctx context.Context, namespaces []string) (*Overview, error) {
	visible := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		visible[ns] = true
	}

	var gates v1alpha1.GateList
	if err := o.Client.List(ctx, &gates); err != nil {
		return nil, fmt.Errorf("listing Gates: %w", err)
	}
	var passages v1alpha1.PassageList
	if err := o.Client.List(ctx, &passages); err != nil {
		return nil, fmt.Errorf("listing Passages: %w", err)
	}

	// Which Gate each running Passage is crossing, so a Gate can name it
	// without a second pass over the Passages for every Gate.
	running := map[string]string{}
	out := &Overview{Namespaces: []NamespaceOverview{}}

	for i := range passages.Items {
		p := &passages.Items[i]
		if !visible[p.Namespace] {
			continue
		}
		switch p.Status.Phase {
		case v1alpha1.PassageRunning, v1alpha1.PassagePending:
			running[p.Namespace+"/"+p.Spec.Gate] = p.Name
			out.Totals.Running++
		case v1alpha1.PassageFailed:
			out.Totals.Failed++
		}
	}

	byNamespace := map[string][]GateSummary{}
	for i := range gates.Items {
		g := &gates.Items[i]
		if !visible[g.Namespace] {
			continue
		}
		s := summarise(g, running[g.Namespace+"/"+g.Name])
		byNamespace[g.Namespace] = append(byNamespace[g.Namespace], s)
		out.Totals.Gates++
		out.Totals.Eligible += s.Eligible
		if s.Suspended {
			out.Totals.Suspended++
		}
		switch s.Health {
		case v1alpha1.HealthHealthy:
			out.Totals.Healthy++
		case v1alpha1.HealthProgressing:
			out.Totals.Progressing++
		case v1alpha1.HealthDegraded:
			out.Totals.Degraded++
		default:
			out.Totals.Unknown++
		}
	}

	for ns, summaries := range byNamespace {
		sort.Slice(summaries, func(a, b int) bool { return summaries[a].Name < summaries[b].Name })
		out.Namespaces = append(out.Namespaces, NamespaceOverview{Namespace: ns, Gates: summaries})
	}
	sort.Slice(out.Namespaces, func(a, b int) bool {
		return out.Namespaces[a].Namespace < out.Namespaces[b].Namespace
	})
	return out, nil
}

func summarise(g *v1alpha1.Gate, runningPassage string) GateSummary {
	s := GateSummary{
		Name: g.Name,
		// Unknown rather than empty when a Gate has not reported: the UI shows
		// the word beside the dot, and an empty one reads as a rendering fault
		// rather than as "this Gate has not said yet".
		Health:    v1alpha1.HealthUnknown,
		Eligible:  len(g.Status.Eligible),
		Running:   runningPassage,
		Suspended: g.Spec.Suspend,
	}
	if g.Status.Health != nil {
		s.Health = g.Status.Health.Status
		s.Issues = g.Status.Health.Issues
	}
	if g.Status.Current != nil {
		s.Current = g.Status.Current.Bundle
	}
	return s
}
