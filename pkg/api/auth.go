// Package api serves Hecate's HTTP API over pkg/ops.
//
// It holds no rules of its own — eligibility, windows, what counts as approved
// are all pkg/ops' (D32) — and it holds no identity of its own either. A caller
// presents the Kubernetes credentials they already have, and Hecate asks the
// Kubernetes API server both who they are and whether they may do the thing.
//
// That is the whole authorisation design. It is not a shortcut: Hecate is a
// Kubernetes controller whose objects are Kubernetes objects, and a second
// permission model over them would be a second answer to "may this person
// promote to production" — with the two disagreeing eventually and silently.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Subject is who is making a request.
type Subject struct {
	Name   string
	Groups []string
}

func (s Subject) String() string { return s.Name }

// Action is one thing a caller might be allowed to do.
//
// The mapping to Kubernetes verbs is the point rather than an implementation
// detail: #74 requires that the right to *cross* and the right to *approve* be
// separable, because an approval a promoter can grant themselves is not an
// approval. They are different verbs on different resources, so a Role can
// carry one without the other — and nothing here has to enforce that, because
// the API server already does.
type Action struct {
	Verb     string
	Resource string
}

var (
	// ActionRead is reading any of the four resources.
	ActionRead = Action{Verb: "list", Resource: "gates"}
	// ActionPromote is asking a Gate to cross a Bundle: it creates a Passage.
	ActionPromote = Action{Verb: "create", Resource: "passages"}
	// ActionApprove is approving a Bundle for a Gate. A distinct resource from
	// the one promoting writes, which is what makes four-eyes real.
	ActionApprove = Action{Verb: "update", Resource: "bundles/status"}
	// ActionAbort stops a running Passage.
	ActionAbort = Action{Verb: "update", Resource: "passages"}
	// ActionPoll asks a Beacon to look at its sources now. A separate verb
	// from reading, so a CI job's identity can be allowed to poke a Beacon
	// without being able to read every Gate in the namespace — which is the
	// whole grant a webhook needs.
	ActionPoll = Action{Verb: "update", Resource: "beacons"}
)

// ErrUnauthenticated means no usable credential was presented.
var ErrUnauthenticated = errors.New("no valid credentials")

// Forbidden is a caller who is known but not permitted.
type Forbidden struct {
	Subject   Subject
	Action    Action
	Namespace string
	Reason    string
}

func (f *Forbidden) Error() string {
	msg := fmt.Sprintf("%s may not %s %s in %s",
		f.Subject.Name, f.Action.Verb, f.Action.Resource, f.Namespace)
	if f.Reason != "" {
		msg += ": " + f.Reason
	}
	return msg
}

// The two permissions the whole design rests on, and the only two the API
// server adds. Detached from the type on purpose: rbac markers are
// package-level, and one attached to a declaration is silently ignored.
//
// Note what is *not* here: impersonate. Hecate asks whether a caller may act;
// it never acts as them. The difference matters if Hecate is ever compromised.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// Authenticator turns a request into a Subject, and answers whether that
// Subject may perform an Action.
type Authenticator struct {
	// Client is Hecate's own client, used to create TokenReviews and
	// SubjectAccessReviews. It needs `create` on those two and nothing more:
	// Hecate asks whether the caller may act, it does not act as them.
	Client client.Client
}

// Authenticate identifies the bearer of a request.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Subject, error) {
	token := bearerToken(r)
	if token == "" {
		return Subject{}, ErrUnauthenticated
	}

	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}
	if err := a.Client.Create(ctx, review); err != nil {
		return Subject{}, fmt.Errorf("reviewing the token: %w", err)
	}
	if !review.Status.Authenticated {
		// The reason is deliberately not returned to the caller: it can
		// distinguish an expired token from an unknown one, which is more than
		// an unauthenticated caller needs to know.
		return Subject{}, ErrUnauthenticated
	}
	return Subject{Name: review.Status.User.Username, Groups: review.Status.User.Groups}, nil
}

// Authorize asks the Kubernetes API server whether the subject may act.
//
// Asked per request rather than cached. A cache here would mean a revoked
// permission still worked for its lifetime, and the whole point of deferring to
// Kubernetes is that its answer is the current one.
func (a *Authenticator) Authorize(ctx context.Context, s Subject, act Action, namespace string) error {
	resource, subresource, _ := strings.Cut(act.Resource, "/")

	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   s.Name,
			Groups: s.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Group:       "hecate.dev",
				Resource:    resource,
				Subresource: subresource,
				Verb:        act.Verb,
			},
		},
	}
	if err := a.Client.Create(ctx, review); err != nil {
		return fmt.Errorf("checking access: %w", err)
	}
	if !review.Status.Allowed || review.Status.Denied {
		return &Forbidden{Subject: s, Action: act, Namespace: namespace, Reason: review.Status.Reason}
	}
	return nil
}

// bearerToken reads the credential a caller presented.
// bearerToken reads the caller's credential from the Authorization header, or
// failing that from the session cookie a browser login set.
//
// One function for both because they carry the same thing: an OIDC ID token
// from the issuer the cluster trusts is a Kubernetes bearer token, so a browser
// session and a `kubectl`-style token are the same credential arriving by
// different routes, and Authenticate below cannot tell them apart. That is the
// point — there is one authorisation path, not one per client.
//
// The header wins, so a script that sets it explicitly is never overridden by a
// cookie the browser happened to attach.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if token, found := strings.CutPrefix(header, "Bearer "); found {
		return strings.TrimSpace(token)
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}
