package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// gateIn is gate() in a namespace of the test's choosing, with a health it
// chooses too — the overview is mostly about reporting those two accurately.
func gateIn(namespace, name string, health v1alpha1.Health) *v1alpha1.Gate {
	g := gate(name)
	g.Namespace = namespace
	g.Status.Health = &v1alpha1.HealthReport{Status: health}
	return g
}

func getOverview(t *testing.T, s *Server, token string) ops.Overview {
	t.Helper()
	rec := call(t, s, token, http.MethodGet, "/api/v1alpha1/overview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview returned %d: %s", rec.Code, rec.Body.String())
	}
	var got ops.Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("overview is not JSON: %v", err)
	}
	return got
}

func TestOverviewShowsOnlyNamespacesTheCallerMayRead(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		// May read acme. May not read finance.
		grants{"ada": {"list gates": true, "list gates in finance": false}},
		gateIn("acme", "production", v1alpha1.HealthHealthy),
		gateIn("finance", "payments", v1alpha1.HealthDegraded),
		// A failed Passage in the namespace the caller may not read. Without
		// this the Gate filter is tested and the Passage filter is not, and a
		// count of failures somewhere the caller cannot look would leak
		// unnoticed — the first version of this test had exactly that gap.
		&v1alpha1.Passage{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Namespace: "finance"},
			Spec:       v1alpha1.PassageSpec{Gate: "payments", Bundle: "payments-1"},
			Status:     v1alpha1.PassageStatus{Phase: v1alpha1.PassageFailed},
		},
	)

	got := getOverview(t, s, "tok")

	if len(got.Namespaces) != 1 || got.Namespaces[0].Namespace != "acme" {
		t.Fatalf("overview shows %+v, want acme alone", got.Namespaces)
	}
	// Not merely hidden from the list — absent from the counts as well. A total
	// that included a namespace the caller cannot open tells them something is
	// wrong somewhere they are not allowed to look, which is worse than not
	// telling them at all.
	if got.Totals.Gates != 1 || got.Totals.Degraded != 0 {
		t.Errorf("totals are %+v, want 1 gate and no degraded", got.Totals)
	}
	// Passages are filtered too, not only Gates. "one thing has failed" is a
	// fact about a namespace, and reporting it to someone who cannot open that
	// namespace tells them something is wrong where they may not look.
	if got.Totals.Failed != 0 {
		t.Errorf("totals report %d failed, want 0 — a Passage leaked from finance", got.Totals.Failed)
	}
	if strings.Contains(mustJSON(t, got), "payments") {
		t.Error("overview names a Gate in a namespace the caller may not read")
	}
}

func TestOverviewCountsHealthAcrossNamespaces(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gateIn("acme", "production", v1alpha1.HealthHealthy),
		gateIn("acme", "staging", v1alpha1.HealthDegraded),
		gateIn("team-b", "production", v1alpha1.HealthProgressing),
	)

	got := getOverview(t, s, "tok")

	if len(got.Namespaces) != 2 {
		t.Fatalf("got %d namespaces, want 2", len(got.Namespaces))
	}
	// Sorted, because a board that reorders itself between loads is one people
	// mis-read at a glance.
	if got.Namespaces[0].Namespace != "acme" || got.Namespaces[1].Namespace != "team-b" {
		t.Errorf("namespaces are %q, %q — want them sorted",
			got.Namespaces[0].Namespace, got.Namespaces[1].Namespace)
	}
	want := ops.Totals{Gates: 3, Healthy: 1, Degraded: 1, Progressing: 1}
	if got.Totals != want {
		t.Errorf("totals are %+v, want %+v", got.Totals, want)
	}
}

func TestOverviewReportsAGateThatHasNotSpokenAsUnknown(t *testing.T) {
	g := gate("production")
	g.Status.Health = nil // never reconciled

	s, _ := newServer(t, map[string]string{"tok": "ada"}, grants{"ada": {"list gates": true}}, g)

	got := getOverview(t, s, "tok")

	if len(got.Namespaces) != 1 || len(got.Namespaces[0].Gates) != 1 {
		t.Fatalf("overview is %+v", got.Namespaces)
	}
	// Not empty. The UI prints the word beside the dot, and a blank reads as a
	// rendering fault rather than as "this Gate has not said yet".
	if h := got.Namespaces[0].Gates[0].Health; h != v1alpha1.HealthUnknown {
		t.Errorf("health is %q, want Unknown", h)
	}
	if got.Totals.Unknown != 1 {
		t.Errorf("totals are %+v, want one unknown", got.Totals)
	}
}

func TestOverviewNamesTheRunningPassageAndCountsFailures(t *testing.T) {
	inFlight := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1-production", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "app-1"},
		Status:     v1alpha1.PassageStatus{Phase: v1alpha1.PassageRunning},
	}
	broke := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "app-0-staging", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "staging", Bundle: "app-0"},
		Status:     v1alpha1.PassageStatus{Phase: v1alpha1.PassageFailed},
	}

	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gateIn("acme", "production", v1alpha1.HealthProgressing),
		gateIn("acme", "staging", v1alpha1.HealthHealthy),
		inFlight, broke,
	)

	got := getOverview(t, s, "tok")

	gates := got.Namespaces[0].Gates
	// The Passage is attached to the Gate it is crossing, not to whichever Gate
	// happened to be read first.
	if gates[0].Name != "production" || gates[0].Running != "app-1-production" {
		t.Errorf("production shows running %q, want app-1-production", gates[0].Running)
	}
	if gates[1].Running != "" {
		t.Errorf("staging shows running %q, want nothing", gates[1].Running)
	}
	if got.Totals.Running != 1 || got.Totals.Failed != 1 {
		t.Errorf("totals are %+v, want 1 running and 1 failed", got.Totals)
	}
}

func TestOverviewReportsASuspendedGate(t *testing.T) {
	g := gateIn("acme", "production", v1alpha1.HealthHealthy)
	g.Spec.Suspend = true

	s, _ := newServer(t, map[string]string{"tok": "ada"}, grants{"ada": {"list gates": true}}, g)

	got := getOverview(t, s, "tok")

	// A suspended Gate is healthy and admitting nothing, which is the one
	// combination a health dot alone describes wrongly.
	if !got.Namespaces[0].Gates[0].Suspended || got.Totals.Suspended != 1 {
		t.Errorf("suspension not reported: %+v", got)
	}
}

func TestOverviewNeedsAuthentication(t *testing.T) {
	s, _ := newServer(t, map[string]string{"tok": "ada"}, grants{"ada": {"list gates": true}})

	rec := call(t, s, "", http.MethodGet, "/api/v1alpha1/overview", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("overview returned %d, want 401", rec.Code)
	}
}

func TestOverviewIsEmptyRatherThanForbiddenForANewUser(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {}}, // may read nothing at all
		gateIn("acme", "production", v1alpha1.HealthHealthy),
	)

	rec := call(t, s, "tok", http.MethodGet, "/api/v1alpha1/overview", "")
	// Filtering, not refusing. A cluster-wide view that 403s because one
	// namespace is out of reach is useless to exactly the team-scoped operators
	// it should serve — and here the caller can reach none, which is an empty
	// board, not an error.
	if rec.Code != http.StatusOK {
		t.Fatalf("overview returned %d, want 200 with nothing in it", rec.Code)
	}
	var got ops.Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Namespaces) != 0 || got.Totals.Gates != 0 {
		t.Errorf("overview is %+v, want empty", got)
	}
	// Never null: the UI maps over this directly.
	if strings.Contains(rec.Body.String(), `"namespaces":null`) {
		t.Error("namespaces serialised as null, which the UI cannot map over")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
