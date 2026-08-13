package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

var base = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// grants is a stand-in cluster RBAC: which subjects hold which permissions.
// Keyed the way a SubjectAccessReview asks — verb on resource — because the
// question this file is really testing is whether the server asks it at all.
type grants map[string]map[string]bool

// asked records every SubjectAccessReview the server made, so a test can assert
// on the question and not only the answer. A server that authorised the wrong
// verb would pass every allow/deny test and still be wrong.
type asked struct {
	reviews []authorizationv1.SubjectAccessReviewSpec
}

func (a *asked) last() authorizationv1.SubjectAccessReviewSpec {
	return a.reviews[len(a.reviews)-1]
}

// newServer builds a Server whose Kubernetes client answers TokenReview and
// SubjectAccessReview the way a real API server would.
func newServer(t *testing.T, tokens map[string]string, rbac grants, objs ...client.Object) (*Server, *asked) {
	t.Helper()

	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, scheme.AddToScheme} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}

	log := &asked{}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption,
			) error {
				switch review := obj.(type) {
				case *authenticationv1.TokenReview:
					if user, ok := tokens[review.Spec.Token]; ok {
						review.Status = authenticationv1.TokenReviewStatus{
							Authenticated: true,
							User:          authenticationv1.UserInfo{Username: user},
						}
					} else {
						review.Status = authenticationv1.TokenReviewStatus{
							Authenticated: false, Error: "unknown token",
						}
					}
					return nil
				case *authorizationv1.SubjectAccessReview:
					log.reviews = append(log.reviews, review.Spec)
					attrs := review.Spec.ResourceAttributes
					resource := attrs.Resource
					if attrs.Subresource != "" {
						resource += "/" + attrs.Subresource
					}
					review.Status = authorizationv1.SubjectAccessReviewStatus{
						Allowed: rbac[review.Spec.User][attrs.Verb+" "+resource],
					}
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	return &Server{
		Ops:  &ops.Ops{Client: c, Now: func() metav1.Time { return metav1.Time{Time: base} }},
		Auth: &Authenticator{Client: c},
		// A cluster this small has no version, and the field exists for /healthz.
		Version: "test",
	}, log
}

// call makes a request as the bearer of a token.
func call(t *testing.T, s *Server, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func gate(name string) *v1alpha1.Gate {
	return &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Admits:  []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{Uses: "flux-wait"}}},
		},
	}
}

func bundle(name string) *v1alpha1.Bundle {
	return &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "acme",
			CreationTimestamp: metav1.Time{Time: base.Add(-time.Hour)},
		},
		Spec: v1alpha1.BundleSpec{Beacon: "app"},
	}
}

// ------------------------------------------------------------------ auth ---

func TestNoCredentialIsRejected(t *testing.T) {
	s, _ := newServer(t, nil, nil, gate("staging"))

	rec := call(t, s, "", "GET", "/api/v1alpha1/namespaces/acme/gates", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// Without this a client cannot tell "log in" from "Hecate is broken".
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestAnUnknownTokenIsRejected(t *testing.T) {
	s, _ := newServer(t, map[string]string{"good": "alice"}, nil, gate("staging"))

	rec := call(t, s, "forged", "GET", "/api/v1alpha1/namespaces/acme/gates", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The API server's reason can distinguish an expired token from an unknown
	// one, which is more than an unauthenticated caller should learn.
	if strings.Contains(rec.Body.String(), "unknown token") {
		t.Errorf("the authentication failure reason leaked: %s", rec.Body.String())
	}
}

// Authenticated is not authorised. The obvious way to get this wrong is to
// treat a valid token as permission to act.
func TestAKnownCallerWithNoRightsIsForbidden(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {}},
		gate("staging"))

	rec := call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/gates", "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if len(log.reviews) != 1 {
		t.Fatalf("made %d access reviews, want 1", len(log.reviews))
	}
}

// The server must ask about the caller, not about itself. Asking with Hecate's
// own identity would authorise everything, and every allow/deny test above
// would still pass.
func TestTheAccessReviewNamesTheCaller(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"list gates": true}},
		gate("staging"))

	call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/gates", "")

	spec := log.last()
	if spec.User != "alice" {
		t.Errorf("access review asked about %q, want alice", spec.User)
	}
	if spec.ResourceAttributes.Namespace != "acme" {
		t.Errorf("access review namespace = %q, want acme", spec.ResourceAttributes.Namespace)
	}
	if spec.ResourceAttributes.Group != "hecate.dev" {
		t.Errorf("access review group = %q", spec.ResourceAttributes.Group)
	}
}

// A namespace in the path must be the namespace authorised. Authorising in one
// namespace and reading in another is the classic version of this bug.
func TestAuthorizationIsScopedToThePathNamespace(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"list gates": true}},
		gate("staging"))

	call(t, s, "t", "GET", "/api/v1alpha1/namespaces/other/gates", "")

	if got := log.last().ResourceAttributes.Namespace; got != "other" {
		t.Errorf("authorised in %q but the request was for other", got)
	}
}

// --------------------------------------------------- separability (#74) ---

// The requirement #74 states: "crossing rights and approval rights must be
// separable, or four-eyes approval is theatre". Not a property of this code so
// much as of the mapping — promote and approve are different verbs on different
// resources, so a Role can carry one without the other.
//
// This is the test that has to hold for the whole design to mean anything.
func TestPromotingDoesNotConferApproving(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"create passages": true}}, // and nothing else
		gate("staging"), bundle("app-1"))

	promote := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/gates/staging/promote", `{"bundle":"app-1"}`)
	if promote.Code == http.StatusForbidden {
		t.Fatalf("a promoter could not promote: %s", promote.Body)
	}

	approve := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/bundles/app-1/approve", `{"gate":"staging"}`)
	if approve.Code != http.StatusForbidden {
		t.Fatalf("the promoter approved their own bundle: %d %s", approve.Code, approve.Body)
	}
}

// And the converse, so the separation is not an artefact of one direction.
func TestApprovingDoesNotConferPromoting(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "bob"},
		grants{"bob": {"update bundles/status": true}},
		gate("staging"), bundle("app-1"))

	approve := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/bundles/app-1/approve", `{"gate":"staging"}`)
	if approve.Code != http.StatusOK {
		t.Fatalf("an approver could not approve: %d %s", approve.Code, approve.Body)
	}

	promote := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/gates/staging/promote", `{"bundle":"app-1"}`)
	if promote.Code != http.StatusForbidden {
		t.Fatalf("the approver promoted: %d %s", promote.Code, promote.Body)
	}
}

// Reading is not acting. A viewer must not be able to promote.
func TestReadingDoesNotConferWriting(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "carol"},
		grants{"carol": {"list gates": true}},
		gate("staging"), bundle("app-1"))

	if rec := call(t, s, "t", "GET",
		"/api/v1alpha1/namespaces/acme/gates", ""); rec.Code != http.StatusOK {
		t.Fatalf("a viewer could not read: %d", rec.Code)
	}
	if rec := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/gates/staging/promote",
		`{"bundle":"app-1"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer promoted: %d %s", rec.Code, rec.Body)
	}
}

// Each write authorises its own action, checked against the mapping rather than
// against itself — a test that read the constants would pass if they were wrong.
func TestEachWriteAsksItsOwnQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		want                     string
	}{
		{"promote", "POST", "/api/v1alpha1/namespaces/acme/gates/staging/promote",
			`{"bundle":"app-1"}`, "create passages"},
		{"approve", "POST", "/api/v1alpha1/namespaces/acme/bundles/app-1/approve",
			`{"gate":"staging"}`, "update bundles/status"},
		{"abort", "POST", "/api/v1alpha1/namespaces/acme/passages/p-1/abort",
			"", "update passages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, log := newServer(t, map[string]string{"t": "alice"}, nil, gate("staging"), bundle("app-1"))
			call(t, s, "t", tc.method, tc.path, tc.body)

			attrs := log.last().ResourceAttributes
			got := attrs.Verb + " " + attrs.Resource
			if attrs.Subresource != "" {
				got += "/" + attrs.Subresource
			}
			if got != tc.want {
				t.Errorf("authorised %q, want %q", got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------- acts ---

// The recorded actor is the authenticated caller. A body-supplied actor would
// make every approval record a claim rather than a fact.
func TestTheActorIsTheCallerNotTheBody(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "bob"},
		grants{"bob": {"update bundles/status": true}},
		gate("staging"), bundle("app-1"))

	// The body says alice; the token says bob.
	rec := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/bundles/app-1/approve",
		`{"gate":"staging","actor":"alice"}`)

	// Strict decoding refuses the unknown field outright, which is the stronger
	// answer: there is no way to even ask.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an unknown field: %s", rec.Code, rec.Body)
	}

	rec = call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/bundles/app-1/approve", `{"gate":"staging"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["approvedBy"] != "bob" {
		t.Errorf("approvedBy = %v, want bob", body["approvedBy"])
	}
}

// A refusal is an answer, not a malfunction, and a client has to be able to
// tell them apart: 409 means "try again when the state changes", 500 does not.
func TestARefusalIsNotAServerError(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"create passages": true}},
		gate("staging"))

	rec := call(t, s, "t", "POST",
		"/api/v1alpha1/namespaces/acme/gates/staging/promote", `{"bundle":""}`)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
}

func TestAMissingObjectIsNotFound(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"list gates": true}})

	rec := call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/gates/ghost", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// A probe is not a caller: requiring a credential to answer it would make an
// outage look like a crash loop.
func TestHealthzNeedsNoCredential(t *testing.T) {
	s, _ := newServer(t, nil, nil)

	rec := call(t, s, "", "GET", "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "test") {
		t.Errorf("healthz does not report the version: %s", rec.Body)
	}
}

// Reads come back as the API types, not a second data model (D32) — a UI that
// deserialises a Gate must get a Gate.
func TestReadsReturnTheAPITypes(t *testing.T) {
	s, _ := newServer(t,
		map[string]string{"t": "alice"},
		grants{"alice": {"list gates": true}},
		gate("staging"))

	rec := call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/gates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var gates []v1alpha1.Gate
	if err := json.Unmarshal(rec.Body.Bytes(), &gates); err != nil {
		t.Fatalf("the response did not decode as Gates: %v\n%s", err, rec.Body)
	}
	if len(gates) != 1 || gates[0].Name != "staging" {
		t.Fatalf("gates = %+v", gates)
	}
	if gates[0].Spec.Admits[0].From.Beacon != "app" {
		t.Error("the spec did not survive the round trip")
	}
}

// #72 asks for all four resources, and this file originally covered three:
// Beacons were missing from pkg/ops entirely, so no amount of testing the API
// in isolation would have found it. Table-driven over the four rather than one
// more happy path, because the failure mode was an omission, not a bug.
func TestAllFourResourcesAreReachable(t *testing.T) {
	objs := []client.Object{
		&v1alpha1.Beacon{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "acme"},
			Spec: v1alpha1.BeaconSpec{Watch: []v1alpha1.WatchSource{
				{Image: &v1alpha1.ImageWatch{Repo: "example.test/api"}},
			}},
		},
		gate("staging"),
		bundle("app-1"),
		&v1alpha1.Passage{
			ObjectMeta: metav1.ObjectMeta{Name: "p-1", Namespace: "acme"},
			Spec:       v1alpha1.PassageSpec{Gate: "staging", Bundle: "app-1"},
		},
	}

	for _, tc := range []struct{ plural, name string }{
		{"beacons", "app"},
		{"gates", "staging"},
		{"bundles", "app-1"},
		{"passages", "p-1"},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			s, _ := newServer(t,
				map[string]string{"t": "alice"},
				grants{"alice": {"list gates": true}},
				objs...)

			list := call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/"+tc.plural, "")
			if list.Code != http.StatusOK {
				t.Fatalf("list: %d %s", list.Code, list.Body)
			}
			var items []map[string]any
			if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil {
				t.Fatalf("list did not decode: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("listed %d, want 1", len(items))
			}

			get := call(t, s, "t", "GET", "/api/v1alpha1/namespaces/acme/"+tc.plural+"/"+tc.name, "")
			if get.Code != http.StatusOK {
				t.Fatalf("get: %d %s", get.Code, get.Body)
			}
		})
	}
}

// Every list endpoint must answer `[]` rather than `null` when there is
// nothing to list.
//
// A nil Go slice marshals to `null`, which is not an empty list to any client:
// the UI did `passages.length` and threw, in a namespace with no Passages —
// which is the state every namespace starts in. One endpoint had it wrong and
// four had it right, so the check is over all of them rather than the one that
// broke.
func TestListEndpointsNeverAnswerNull(t *testing.T) {
	// Reuses the same construction as every other test here, so the fake
	// client decodes the same way a real one does.
	s, _ := newServer(t, map[string]string{"t": "someone"}, grants{})

	for _, path := range []string{"beacons", "gates", "bundles", "passages"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/v1alpha1/namespaces/empty/"+path, nil)
			r.SetPathValue("namespace", "empty")

			var (
				out any
				err error
			)
			switch path {
			case "beacons":
				out, err = s.listBeacons(r.Context(), Subject{}, r)
			case "gates":
				out, err = s.listGates(r.Context(), Subject{}, r)
			case "bundles":
				out, err = s.listBundles(r.Context(), Subject{}, r)
			case "passages":
				out, err = s.listPassages(r.Context(), Subject{}, r)
			}
			if err != nil {
				t.Fatal(err)
			}

			writeJSON(rec, http.StatusOK, out)
			if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
				t.Errorf("%s answered %s, want []", path, got)
			}
		})
	}
}

func beacon(name string) *v1alpha1.Beacon {
	return &v1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec: v1alpha1.BeaconSpec{
			Watch: []v1alpha1.WatchSource{{Image: &v1alpha1.ImageWatch{Repo: "ghcr.io/acme/app"}}},
		},
	}
}

// The inbound webhook (#102). No shared secret and no HMAC: the caller presents
// a bearer token and Kubernetes says whether it is anybody, which is the
// posture Flux v2.9 moved to with OIDC-secured Receivers.
func TestPollingABeaconNeedsWritePermissionOnBeacons(t *testing.T) {
	s, log := newServer(t,
		map[string]string{"ci-token": "ci@acme.example", "reader-token": "reader@acme.example"},
		grants{
			"ci@acme.example":     {"update beacons": true},
			"reader@acme.example": {"list gates": true},
		},
		beacon("app"))

	rec := call(t, s, "ci-token", "POST", "/api/v1alpha1/namespaces/acme/beacons/app/poll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	// The question asked matters as much as the answer: authorising the wrong
	// verb would pass an allow test and still hand out the wrong permission.
	if got := log.last().ResourceAttributes; got.Verb != "update" || got.Resource != "beacons" {
		t.Errorf("asked %s %s, want update beacons", got.Verb, got.Resource)
	}

	// Reading is a different grant. A CI job that may poke a Beacon must not
	// thereby be able to read every Gate in the namespace, and vice versa.
	rec = call(t, s, "reader-token", "POST", "/api/v1alpha1/namespaces/acme/beacons/app/poll", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("a caller with only read access polled a Beacon: %d", rec.Code)
	}
}

func TestPollingIsRefusedWithoutCredentials(t *testing.T) {
	s, _ := newServer(t, map[string]string{}, grants{}, beacon("app"))

	rec := call(t, s, "", "POST", "/api/v1alpha1/namespaces/acme/beacons/app/poll", "")
	// A webhook endpoint that answered an unauthenticated caller would be an
	// open door to every Beacon in the cluster.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}
