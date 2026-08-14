package api

import (
	"net/http"
	"strings"
	"testing"
)

/**
 * These cover the settings writes, and the first two are the reason the file
 * exists.
 *
 * hecate-api writes with its own ServiceAccount. Kubernetes' built-in
 * escalation prevention compares the *writer's* rights against what is being
 * granted, and the writer here is the server rather than the person clicking —
 * so the protection everyone assumes is present is not. What stands in for it
 * is an explicit check of the caller, and an allowlist of the roles that may be
 * bound at all.
 */

const rbacGrants = "/api/v1alpha1/rbac/grants"

func TestBindingAnArbitraryRoleIsRefused(t *testing.T) {
	// The failure this prevents: a settings screen that takes a role name from
	// the request is a way to grant yourself cluster-admin, bounded only by
	// what the server's ServiceAccount can grant.
	s, _ := newServer(t,
		map[string]string{"t": "someone@example.com"},
		grants{"someone@example.com": {"create clusterrolebindings": true}},
	)

	for _, role := range []string{"cluster-admin", "edit", "hecate-promoter-but-not-really", ""} {
		body := `{"subject":"someone@example.com","kind":"User","role":"` + role + `"}`
		rec := call(t, s, "t", http.MethodPost, rbacGrants, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("role %q: got %d, want 400 — only the chart's own roles may be bound", role, rec.Code)
		}
	}
}

func TestBindingARoleChecksTheCallerNotTheServer(t *testing.T) {
	// Someone who may read Hecate resources but holds no RBAC rights at all.
	// Without the explicit check they would inherit the server's ability to
	// write ClusterRoleBindings simply by reaching the endpoint.
	s, log := newServer(t,
		map[string]string{"t": "reader@example.com"},
		grants{"reader@example.com": {"list gates": true}},
	)

	rec := call(t, s, "t", http.MethodPost, rbacGrants,
		`{"subject":"reader@example.com","kind":"User","role":"hecate-promoter"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — the caller holds no right to bind roles", rec.Code)
	}

	// The question matters as much as the answer: authorising the wrong
	// resource would refuse the right people and allow the wrong ones.
	last := log.last()
	if last.ResourceAttributes.Resource != "clusterrolebindings" ||
		last.ResourceAttributes.Group != "rbac.authorization.k8s.io" ||
		last.ResourceAttributes.Verb != "create" {
		t.Errorf("asked the wrong question: %+v", last.ResourceAttributes)
	}
}

func TestBindingARoleIsIdempotent(t *testing.T) {
	// Granting the same person the same role twice is someone clicking again,
	// not an error. Reporting a conflict would present success as failure.
	s, _ := newServer(t,
		map[string]string{"t": "admin@example.com"},
		grants{"admin@example.com": {"create clusterrolebindings": true}},
	)

	body := `{"subject":"someone@example.com","kind":"User","role":"hecate-approver"}`
	if rec := call(t, s, "t", http.MethodPost, rbacGrants, body); rec.Code != http.StatusOK {
		t.Fatalf("first grant: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec := call(t, s, "t", http.MethodPost, rbacGrants, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second grant: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Errorf("the second grant should report created=false, got %s", rec.Body.String())
	}
}

func TestConnectingAClusterChecksSecretRights(t *testing.T) {
	// A kubeconfig is the keys to another cluster. Someone who may promote is
	// not thereby someone who may add one.
	s, log := newServer(t,
		map[string]string{"t": "promoter@example.com"},
		grants{"promoter@example.com": {"create passages": true}},
	)

	rec := call(t, s, "t", http.MethodPost, "/api/v1alpha1/namespaces/team/clusters",
		`{"name":"remote","kubeconfig":"apiVersion: v1\nkind: Config\n"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if last := log.last(); last.ResourceAttributes.Resource != "secrets" ||
		last.ResourceAttributes.Group != "" || last.ResourceAttributes.Namespace != "team" {
		t.Errorf("asked the wrong question: %+v", last.ResourceAttributes)
	}
}
