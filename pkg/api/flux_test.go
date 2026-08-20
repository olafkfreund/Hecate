package api

import (
	"encoding/json"
	"net/http"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// watchingFlux is a Gate whose health depends on one Kustomization.
func watchingFlux(gateName, kustomization string) *v1alpha1.Gate {
	g := gate(gateName)
	with, _ := json.Marshal(map[string]any{
		"resources": []map[string]string{{"kind": "Kustomization", "name": kustomization}},
	})
	g.Spec.Watch = []v1alpha1.HealthCheck{
		{Uses: "flux", With: &apiextensionsv1.JSON{Raw: with}},
	}
	return g
}

// kustomization is a Flux object as the cluster holds it.
func kustomization(name, namespace string, suspended bool) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"suspend": suspended},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded",
				"observedGeneration": int64(1),
			}},
			"lastAppliedRevision": "main@sha1:abc",
			"observedGeneration":  int64(1),
		},
	}}
	o.SetGeneration(1)
	return o
}

func fluxOf(t *testing.T, s *Server, token, gateName string) []ops.FluxResource {
	t.Helper()
	rec := call(t, s, token, http.MethodGet,
		"/api/v1alpha1/namespaces/acme/gates/"+gateName+"/flux", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("flux returned %d: %s", rec.Code, rec.Body.String())
	}
	var got []ops.FluxResource
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFluxListsWhatTheGateActuallyWatches(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", false),
		// In the namespace, but nothing watches it. Listing every Kustomization
		// would invite someone to suspend one this Gate has nothing to do with.
		kustomization("someone-elses", "acme", false),
	)

	got := fluxOf(t, s, "tok", "production")

	if len(got) != 1 || got[0].Name != "podinfo" {
		t.Fatalf("flux panel shows %+v, want podinfo alone", got)
	}
	if got[0].Health != v1alpha1.HealthHealthy {
		t.Errorf("health is %q, want Healthy", got[0].Health)
	}
	if got[0].Revision != "main@sha1:abc" {
		t.Errorf("revision is %q, want the applied one", got[0].Revision)
	}
}

func TestFluxReportsASuspendedResource(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", true),
	)

	got := fluxOf(t, s, "tok", "production")

	// The whole reason the panel exists. A suspended Kustomization is healthy
	// by every condition it reports and is applying nothing.
	if !got[0].Suspended {
		t.Errorf("suspension not reported: %+v", got[0])
	}
}

func TestFluxReportsAMissingResourceAsAbsentNotBroken(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true}},
		watchingFlux("production", "not-created-yet"),
	)

	got := fluxOf(t, s, "tok", "production")

	// A Gate committed before its Kustomization exists is ordinary, and
	// reporting it as unhealthy would send someone looking for a fault.
	if len(got) != 1 || !got[0].Missing {
		t.Fatalf("got %+v, want one missing resource", got)
	}
}

func TestSuspendNeedsItsOwnPermission(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"tok": "ada"},
		// May read, and may promote. May not operate Flux.
		grants{"ada": {"list gates": true, "create passages": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", false),
	)

	rec := call(t, s, "tok", http.MethodPost,
		"/api/v1alpha1/namespaces/acme/gates/production/flux/suspend",
		`{"kind":"Kustomization","name":"podinfo","suspend":true}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspend returned %d, want 403", rec.Code)
	}
	// Authorised against Flux's own resource, not a Hecate one: someone who may
	// already patch Kustomizations has this right without Hecate's help, and
	// someone who may not should not gain it by holding a Hecate role.
	attrs := log.last().ResourceAttributes
	if attrs == nil || attrs.Group != "kustomize.toolkit.fluxcd.io" || attrs.Verb != "patch" {
		t.Errorf("authorised %+v, want patch on kustomize.toolkit.fluxcd.io", attrs)
	}
}

func TestSuspendRefusesAResourceTheGateDoesNotWatch(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "patch kustomizations": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", false),
		kustomization("someone-elses", "acme", false),
	)

	rec := call(t, s, "tok", http.MethodPost,
		"/api/v1alpha1/namespaces/acme/gates/production/flux/suspend",
		`{"kind":"Kustomization","name":"someone-elses","suspend":true}`)

	// Scoped to the Gate's own resources. A handler that suspended anything it
	// was named would let a caller stop reconciliation of a resource nobody
	// gave them, simply by asking for it.
	if rec.Code == http.StatusOK {
		t.Fatal("suspended a Kustomization the Gate does not watch")
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kustomization("x", "acme", false).GroupVersionKind())
	if err := s.Ops.Client.Get(t.Context(),
		client.ObjectKey{Namespace: "acme", Name: "someone-elses"}, obj); err != nil {
		t.Fatal(err)
	}
	if suspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend"); suspended {
		t.Error("the untouched Kustomization was suspended anyway")
	}
}

func TestSuspendAndResume(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "patch kustomizations": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", false),
	)

	for _, want := range []bool{true, false} {
		body := `{"kind":"Kustomization","name":"podinfo","suspend":` +
			map[bool]string{true: "true", false: "false"}[want] + `}`
		rec := call(t, s, "tok", http.MethodPost,
			"/api/v1alpha1/namespaces/acme/gates/production/flux/suspend", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("suspend=%v returned %d: %s", want, rec.Code, rec.Body.String())
		}
		if got := fluxOf(t, s, "tok", "production"); got[0].Suspended != want {
			t.Errorf("after asking for suspend=%v the resource reports %v", want, got[0].Suspended)
		}
	}
	// Resuming has to work for whoever can suspend, or an operator can stop
	// reconciliation and not start it again.
}

func TestReconcileStampsAndEchoesIt(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "patch kustomizations": true}},
		watchingFlux("production", "podinfo"),
		kustomization("podinfo", "acme", false),
	)

	rec := call(t, s, "tok", http.MethodPost,
		"/api/v1alpha1/namespaces/acme/gates/production/flux/reconcile",
		`{"kind":"Kustomization","name":"podinfo"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile returned %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		RequestedAt string `json:"requestedAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestedAt == "" {
		t.Fatal("no stamp echoed — a caller cannot tell its own request landed")
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kustomization("x", "acme", false).GroupVersionKind())
	if err := s.Ops.Client.Get(t.Context(),
		client.ObjectKey{Namespace: "acme", Name: "podinfo"}, obj); err != nil {
		t.Fatal(err)
	}
	// The annotation Flux itself watches, and the one `flux reconcile` sets.
	if got := obj.GetAnnotations()["reconcile.fluxcd.io/requestedAt"]; got != body.RequestedAt {
		t.Errorf("annotation is %q, echoed %q", got, body.RequestedAt)
	}
}

func TestFluxSkipsARemoteCluster(t *testing.T) {
	g := gate("production")
	with, _ := json.Marshal(map[string]any{
		"resources":  []map[string]string{{"kind": "Kustomization", "name": "podinfo"}},
		"clusterRef": map[string]string{"name": "elsewhere"},
	})
	g.Spec.Watch = []v1alpha1.HealthCheck{{Uses: "flux", With: &apiextensionsv1.JSON{Raw: with}}}

	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "patch kustomizations": true}},
		g, kustomization("podinfo", "acme", false),
	)

	// Deliberately absent rather than shown against the local cluster, which
	// would report the state of a same-named resource that is not the one the
	// Gate watches — and offer to suspend it.
	if got := fluxOf(t, s, "tok", "production"); len(got) != 0 {
		t.Errorf("remote-cluster resources listed: %+v", got)
	}

	rec := call(t, s, "tok", http.MethodPost,
		"/api/v1alpha1/namespaces/acme/gates/production/flux/suspend",
		`{"kind":"Kustomization","name":"podinfo","suspend":true}`)
	if rec.Code == http.StatusOK {
		t.Error("suspended a local resource standing in for a remote one")
	}
}

func TestFluxNeedsAKindAndAName(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"tok": "ada"},
		grants{"ada": {"list gates": true, "patch kustomizations": true}},
		watchingFlux("production", "podinfo"),
	)

	rec := call(t, s, "tok", http.MethodPost,
		"/api/v1alpha1/namespaces/acme/gates/production/flux/reconcile", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an empty body returned %d, want 400", rec.Code)
	}
}

var _ = metav1.Now
