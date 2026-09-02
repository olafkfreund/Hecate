package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// TestSettingsFailsRatherThanDropAndUnwellNamespace is the mutation check for
// the bug D62 fixes: settings used to `continue` past *any* Authorize error,
// which made a broken API server indistinguishable from an ordinary refusal —
// a team's Fides evidence targets and connected clusters would just vanish
// behind a 200. visibleNamespaces (used by listNamespaces and overview) always
// told the two apart; settings now shares that helper instead of keeping its
// own copy of the loop.
func TestSettingsFailsOnAnUnwellNamespaceRatherThanDroppingIt(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, scheme.AddToScheme} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			&v1alpha1.Gate{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "acme"}},
			&v1alpha1.Gate{ObjectMeta: metav1.ObjectMeta{Name: "g2", Namespace: "broken"}},
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.CreateOption,
			) error {
				switch review := obj.(type) {
				case *authenticationv1.TokenReview:
					review.Status = authenticationv1.TokenReviewStatus{
						Authenticated: true,
						User:          authenticationv1.UserInfo{Username: "alice"},
					}
					return nil
				case *authorizationv1.SubjectAccessReview:
					if review.Spec.ResourceAttributes.Namespace == "broken" {
						// A real API-server fault, not a refusal — nothing in
						// Kubernetes RBAC produces this; it stands in for "the
						// server is unwell right now".
						return errors.New("etcdserver: request timed out")
					}
					review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: true}
					return nil
				}
				return wc.Create(ctx, obj, opts...)
			},
		}).
		Build()

	s := &Server{
		Ops:     &ops.Ops{Client: c, Now: func() metav1.Time { return metav1.Time{Time: base} }},
		Auth:    &Authenticator{Client: c},
		Version: "test",
	}

	rec := call(t, s, "alice", "GET", "/api/v1alpha1/settings", "")

	// Before D62: settings' own Authorize-error loop treated this identically
	// to a plain Forbidden and returned 200 with "broken" simply missing from
	// Fides/Clusters. After: it shares visibleNamespaces, which distinguishes
	// the two and fails the whole request.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s — want 500: an unwell API server must fail the request, "+
			"not silently drop the namespace it could not check", rec.Code, rec.Body)
	}
}

func TestUnreachableExplainsAnExpiredToken(t *testing.T) {
	// What a cluster answers when the kubeconfig's token has run out. The
	// client library renders this as "the server has asked for the client to
	// provide credentials", which reads as "there is no credential here" when
	// in fact there is one that has expired — the commonest way to get a
	// connected cluster wrong, and it fails a day after it is set up.
	unauthorised := apierrors.NewUnauthorized("Unauthorized")

	got := unreachableBecause(unauthorised)

	if !strings.Contains(got, "expired") {
		t.Errorf("a 401 is explained as %q, which does not mention expiry", got)
	}
	if strings.Contains(got, "asked for the client to provide credentials") {
		t.Error("a 401 still passes the client library's own wording through")
	}
}

func TestUnreachableKeepsAnUnfamiliarErrorIntact(t *testing.T) {
	// Guessing at a cause we have not recognised would replace a true message
	// with a plausible wrong one, which is worse than being unhelpful.
	got := unreachableBecause(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))

	if got != "dial tcp 10.0.0.1:443: i/o timeout" {
		t.Errorf("an unrecognised error was rewritten to %q", got)
	}
}

func TestHomeClusterReportsWhereHecateRuns(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	got := homeCluster()

	if !got.InCluster {
		t.Error("running in a cluster was not reported as such")
	}
	// The service address, which is what this process actually dials — not the
	// public endpoint someone might expect. Honest beats familiar here.
	if got.Server != "https://10.0.0.1:443" {
		t.Errorf("server is %q, want https://10.0.0.1:443", got.Server)
	}
}

func TestHomeClusterSaysNothingWhenNotInACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	got := homeCluster()

	// A developer running the API against their own kubeconfig. Inventing an
	// address would be worse than saying nothing, because the screen would then
	// claim a connection that is not the one being used.
	if got.InCluster || got.Server != "" {
		t.Errorf("got %+v, want an empty home cluster", got)
	}
}

func TestHomeClusterHandlesAnIPv6ServiceAddress(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	got := homeCluster()

	// Bracketed, or the URL is unparseable. Concatenating host and port with a
	// colon is the obvious spelling and produces https://fd00::1:443.
	if got.Server != "https://[fd00::1]:443" {
		t.Errorf("server is %q, want https://[fd00::1]:443", got.Server)
	}
}

func TestDedupeSortsAndRemovesRepeats(t *testing.T) {
	// Written against the hand-rolled implementation before replacing it with
	// slices.Sort + slices.Compact, so "the replacement is equivalent" is a
	// thing the suite can answer rather than a thing I asserted.
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"repeats collapse", []string{"b", "a", "b", "a"}, []string{"a", "b"}},
		{"already unique still sorts", []string{"c", "a", "b"}, []string{"a", "b", "c"}},
		{"single value", []string{"only"}, []string{"only"}},
		// Empty answers nil, which matters only because the field is omitempty
		// and both spellings serialise away — recorded so a change to it is a
		// decision rather than a surprise.
		{"empty", []string{}, nil},
		{"nil", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupe(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupe(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupe(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
