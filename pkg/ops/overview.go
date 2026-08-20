package ops

import (
	"context"
	"fmt"
	"sort"
	"time"

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
	// Activity is what crossed and what failed, by day, oldest first.
	Activity []Day `json:"activity"`
}

// Day is one day's crossings and failures.
//
// Built from Gate.status.history rather than from Passages: a Gate keeps a
// bounded number of Passages and the detail ages out, while history is capped
// but long-lived and survives the Passage it names. A trend drawn from
// Passages would quietly shorten itself as retention collected them, which is
// the one thing a trend must not do.
type Day struct {
	// Date is the day in YYYY-MM-DD, in UTC. A chart axis needs a stable label
	// and the server is the only place that knows what "today" means for the
	// data it just counted.
	Date string `json:"date"`
	// Crossed is how many Bundles entered a Gate that day.
	Crossed int `json:"crossed"`
	// Failed is how many Passages ended badly that day.
	//
	// Subject to Passage retention, unlike Crossed — a failure that has been
	// collected is no longer counted. Better than not drawing failures at all,
	// but it means the further back the chart goes the more it flatters.
	Failed int `json:"failed"`
}

// activityDays is how far back the chart looks.
//
// Two weeks: long enough to show a bad week against a good one, short enough
// that Passage retention has probably not eaten the failures yet.
const activityDays = 14

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
	crossedOn := map[string]int{}
	failedOn := map[string]int{}
	out := &Overview{Namespaces: []NamespaceOverview{}, Activity: []Day{}}

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
			// FinishedAt, not the creation time: a Passage that ran for an hour
			// belongs to the day it gave up on, which is the day someone was
			// looking at it.
			if p.Status.FinishedAt != nil {
				failedOn[day(p.Status.FinishedAt.Time)]++
			}
		}
	}

	byNamespace := map[string][]GateSummary{}
	for i := range gates.Items {
		g := &gates.Items[i]
		if !visible[g.Namespace] {
			continue
		}
		for _, occ := range g.Status.History {
			crossedOn[day(occ.EnteredAt.Time)]++
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
	out.Activity = activity(o.now().Time, crossedOn, failedOn)
	return out, nil
}

// day is the UTC date a time falls on.
//
// UTC rather than the server's zone: the two ends of this are a Kubernetes
// timestamp and a chart axis, and a bucket boundary that moves with whichever
// zone the pod happens to run in makes the same data draw differently in two
// deployments.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// activity fills in every day in the window, including the quiet ones.
//
// A series with gaps draws a chart where a week of nothing looks the same
// width as a day of nothing — the empty days are the shape of the story, not
// missing data, so they are zeroes rather than absences.
func activity(now time.Time, crossed, failed map[string]int) []Day {
	days := make([]Day, 0, activityDays)
	for i := activityDays - 1; i >= 0; i-- {
		d := day(now.AddDate(0, 0, -i))
		days = append(days, Day{Date: d, Crossed: crossed[d], Failed: failed[d]})
	}
	return days
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
