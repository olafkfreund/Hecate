package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// The point of the cluster-wide routes: one call, every namespace, no picker.
func TestClusterWideGatesSpanNamespaces(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		gateIn("acme", "production", v1alpha1.HealthHealthy),
		gateIn("zulu", "staging", v1alpha1.HealthHealthy),
	)

	rec := call(t, s, "tok", "GET", "/api/v1alpha1/gates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var got []v1alpha1.Gate
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d gates, want both namespaces", len(got))
	}
	// Grouped by namespace, so the page can render sections without sorting the
	// list itself — and so it does not reshuffle between loads.
	if got[0].Namespace != "acme" || got[1].Namespace != "zulu" {
		t.Errorf("order is %s,%s — want grouped by namespace",
			got[0].Namespace, got[1].Namespace)
	}
}

// The security property. A route with no namespace in its path must still
// answer only for the namespaces this caller may read — otherwise removing the
// picker would have widened what everyone can see.
func TestClusterWideListsHideNamespacesTheCallerMayNotRead(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {
			"list gates": true, "list gates in finance": false,
			"list bundles": true, "list bundles in finance": false,
			"list beacons": true, "list beacons in finance": false,
			"list passages": true, "list passages in finance": false,
		}},
		gateIn("acme", "production", v1alpha1.HealthHealthy),
		gateIn("finance", "payments", v1alpha1.HealthHealthy),
		&v1alpha1.Bundle{ObjectMeta: metav1.ObjectMeta{Name: "secret-bundle", Namespace: "finance"}},
		&v1alpha1.Beacon{ObjectMeta: metav1.ObjectMeta{Name: "secret-beacon", Namespace: "finance"}},
		&v1alpha1.Passage{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-passage", Namespace: "finance"},
			Spec:       v1alpha1.PassageSpec{Gate: "payments", Bundle: "secret-bundle"},
		},
	)

	for _, path := range []string{
		"/api/v1alpha1/gates",
		"/api/v1alpha1/bundles",
		"/api/v1alpha1/beacons",
		"/api/v1alpha1/passages",
		"/api/v1alpha1/audit",
	} {
		rec := call(t, s, "tok", "GET", path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d: %s", path, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		// Checked against the rendered JSON rather than a decoded field, so a
		// name leaking through any part of the payload is caught — including
		// somewhere this test does not know to look.
		if strings.Contains(body, "finance") || strings.Contains(body, "secret-") {
			t.Errorf("%s leaked a namespace the caller may not read: %s", path, body)
		}
	}
}

// A new user who may read nothing gets an empty list, not a 403. Refusing them
// the page entirely reads as Hecate being broken rather than as them having no
// access yet.
func TestClusterWideListsAreEmptyRatherThanForbidden(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "newbie"},
		grants{},
		gateIn("acme", "production", v1alpha1.HealthHealthy),
	)

	rec := call(t, s, "tok", "GET", "/api/v1alpha1/gates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty list", rec.Code)
	}
	var got []v1alpha1.Gate
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d gates, want none", len(got))
	}
}

func TestClusterWideListsNeedACredential(t *testing.T) {
	s, _ := newServer(t, map[string]string{"tok": "ada"}, grants{"ada": {"list gates": true}})

	if rec := call(t, s, "", "GET", "/api/v1alpha1/gates", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// Audit carries the namespace on every entry, because a trail spanning
// namespaces has no other way to say where something happened.
func TestClusterWideAuditNamesTheNamespace(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "list passages": true, "list bundles": true}},
		// A Gate as well as the Passage: visible namespaces are discovered from
		// those holding Gates or Beacons, so a namespace with only a Passage in
		// it is one nobody can see.
		gateIn("acme", "production", v1alpha1.HealthHealthy),
		&v1alpha1.Passage{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "acme"},
			Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "b1"},
			Status:     v1alpha1.PassageStatus{Phase: v1alpha1.PassageSucceeded},
		},
	)

	rec := call(t, s, "tok", "GET", "/api/v1alpha1/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Namespace string `json:"namespace"`
		Gate      string `json:"gate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no audit entries")
	}
	if got[0].Namespace != "acme" {
		t.Errorf("namespace = %q, want acme — an entry that cannot say where it happened", got[0].Namespace)
	}
}
