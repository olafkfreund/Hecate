package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/ops"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

// Server is Hecate's HTTP API.
type Server struct {
	Ops  *ops.Ops
	Auth *Authenticator
	// Version is reported at /healthz, so an operator can tell which build is
	// answering without reading the deployment.
	Version string
	// Login is the browser sign-in flow. Nil serves the API to callers who
	// already hold a Kubernetes token — which is every CLI and script, and is
	// why this is optional rather than required.
	Login *Login
	// Steps is the same step Registry the controller validates a Gate's step
	// list against, so authorPassage refuses the same `with:` blocks admission
	// would (hecate#172 scope item 4). Nil skips the check rather than
	// panicking — every production wiring sets it; a test that only needs the
	// rest of the route table should not have to build one.
	Steps *passage.Registry

	// providers builds a git host's API client. Nil is provider.New; a test
	// overrides it to avoid a network call, the same seam GitPullRequest uses.
	providers func(provider.Kind, provider.Config) (provider.Provider, error)
	// publish clones a repository, commits one file and pushes a new branch.
	// Nil is gitPublish; a test overrides it so authorPassage's request
	// handling can be checked without a real git remote.
	publish func(context.Context, publishRequest) (string, error)
}

// Handler returns the routes.
//
// Every route is authenticated, and every route that changes something is
// authorised for its own action — the reads and the writes do not share a
// permission, and promoting and approving do not share one either (#74).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated, deliberately: a liveness probe is not a caller, and
	// requiring a credential to answer it means an outage looks like a crash.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
	})

	// Unauthenticated by necessity: these are how a caller *becomes*
	// authenticated. They are safe because neither issues anything without a
	// code the provider will only send to the registered redirect URL, and a
	// state this server set.
	if s.Login != nil {
		s.Login.Routes(mux)
	}

	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/beacons",
		s.guard(ActionRead, s.listBeacons))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/beacons/{name}",
		s.guard(ActionRead, s.getBeacon))
	// Which namespaces this caller can see anything in. Deliberately not behind
	// guard(): guard authorises against the namespace in the path, and this
	// route has none, so it would ask "may you read gates cluster-wide?" — a
	// right a team-scoped operator has no reason to hold, and refusing them the
	// list of their own namespaces is precisely backwards.
	mux.Handle("GET /api/v1alpha1/namespaces", s.authenticated(s.listNamespaces))

	// Settings reads across namespaces and filters per namespace, same as the
	// namespace list, so it is authenticated here and authorised in the handler.
	mux.Handle("GET /api/v1alpha1/settings", s.authenticated(s.settings))

	// The step catalogue and what each step's `with:` block accepts. Same answer
	// in every namespace, so it is not addressed by one.
	mux.Handle("GET /api/v1alpha1/steps", s.authenticated(s.stepSchemas))

	// Cluster-wide lists: every namespace the caller can read, in one call.
	// What the screens use, now that they show everything rather than one
	// namespace picked from a list. See clusterwide.go for why these are
	// authenticated rather than guarded.
	mux.Handle("GET /api/v1alpha1/gates", s.authenticated(s.allGates))
	mux.Handle("GET /api/v1alpha1/bundles", s.authenticated(s.allBundles))
	mux.Handle("GET /api/v1alpha1/beacons", s.authenticated(s.allBeacons))
	mux.Handle("GET /api/v1alpha1/passages", s.authenticated(s.allPassages))
	mux.Handle("GET /api/v1alpha1/audit", s.authenticated(s.allAudit))

	// Every Gate the caller can see, in every namespace they can see it in.
	// Authenticated rather than guarded: see the note on overview.
	mux.Handle("GET /api/v1alpha1/overview", s.authenticated(s.overview))

	// Settings writes. Each authorises the CALLER against the exact resource it
	// touches, inside the handler, because guard() checks hecate.dev resources
	// in a path namespace and these are RBAC bindings, core Secrets and a
	// cluster-scoped grant. The server writes with its own ServiceAccount, so
	// skipping that check would make every one of these a way to borrow the
	// server's permissions.
	mux.Handle("GET /api/v1alpha1/rbac/grants", s.authenticated(s.listBindings))
	mux.Handle("POST /api/v1alpha1/rbac/grants", s.authenticated(s.bindRole))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/clusters", s.authenticated(s.connectCluster))
	mux.Handle("PUT /api/v1alpha1/namespaces/{namespace}/gates/{name}/evidence", s.authenticated(s.setEvidence))

	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/gates",
		s.guard(ActionRead, s.listGates))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/gates/{name}",
		s.guard(ActionRead, s.getGate))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/gates/{name}/explain",
		s.guard(ActionRead, s.explainGate))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/bundles",
		s.guard(ActionRead, s.listBundles))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/bundles/{name}",
		s.guard(ActionRead, s.getBundle))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/bundles/{name}/evidence",
		s.guard(ActionRead, s.bundleEvidence))
	// The audit trail. Guarded as an ordinary namespace read: it is assembled
	// from Gates, Passages and Bundles, and someone who may read those three
	// may read a summary of them.
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/audit",
		s.guard(ActionRead, s.auditTrail))

	// Server-sent events: "something in this namespace changed". Read access,
	// because that is what it lets you infer.
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/watch",
		s.stream(ActionRead, s.watchNamespace))

	// The same stream, cluster-wide: "something you can see changed". The
	// handler needs no variant — PathValue("namespace") is "" on this route,
	// and an empty namespace is already how a List means every namespace.
	mux.Handle("GET /api/v1alpha1/watch", s.streamAuthenticated(s.watchNamespace))

	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/passages",
		s.guard(ActionRead, s.listPassages))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/passages/{name}",
		s.guard(ActionRead, s.getPassage))

	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/beacons/{name}/poll",
		s.guard(ActionPoll, s.pollBeacon))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/gates/{name}/promote",
		s.guard(ActionPromote, s.promote))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/bundles/{name}/approve",
		s.guard(ActionApprove, s.approve))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/passages/{name}/abort",
		s.guard(ActionAbort, s.abort))

	// Author a Passage: render the composed step list to YAML, commit it to a
	// branch, and open a pull request through pkg/provider (hecate#172 stage
	// 2). Never touches the cluster — see authorPassage.
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/passages/author",
		s.guard(ActionAuthorPassage, s.authorPassage))

	// Validate a step list without opening anything: the form's live
	// feedback, using the same Registry.Validate authorPassage refuses on
	// (hecate#172 scope item 4, D60). Authenticated only, like /steps below —
	// it reads no cluster state, so there is no namespace to guard() against
	// and no write for a caller to be trusted with.
	mux.Handle("POST /api/v1alpha1/passages/validate", s.authenticated(s.validatePassage))

	// What a Gate's crossings actually depend on, and the three things an
	// operator does to it when something is wrong.
	// Would the eligible Bundles actually cross? Read access, because it is a
	// question about evidence that already exists rather than a change to any.
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/gates/{name}/preflight",
		s.guard(ActionRead, s.preflight))

	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/gates/{name}/flux",
		s.guard(ActionRead, s.fluxResources))
	// The two Flux writes authorise inside the handler rather than through
	// guard(), because the Action is not a constant: what gets patched is
	// whatever kind the Gate's watch names, and that is only known once the
	// Gate has been read. Both handlers resolve first and authorise against
	// the resolved group and resource — see authorizeFluxWrite.
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/gates/{name}/flux/suspend",
		s.authenticated(s.suspendFlux))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/gates/{name}/flux/reconcile",
		s.authenticated(s.reconcileFlux))

	// Last, and on "/" so it catches everything the routes above did not.
	// Go's mux prefers the more specific pattern, so /api/... and /healthz
	// still reach their handlers — the UI only sees what is left.
	//
	// Unauthenticated, deliberately: these are HTML, CSS and JavaScript, not
	// data. The application they make up cannot read a single Gate without a
	// credential, and requiring one to fetch the sign-in page would leave
	// nowhere to sign in from.
	mux.Handle("GET /", s.uiHandler())

	return mux
}

// handler is a route that already knows who is calling.
type handler func(ctx context.Context, s Subject, r *http.Request) (any, error)

// guard authenticates, authorises, and then runs the handler.
//
// Both checks happen before the handler, so a route cannot forget one: the only
// way to add a route is through here, and it takes the Action as an argument
// rather than deriving it.
func (s *Server) guard(action Action, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		subject, err := s.Auth.Authenticate(ctx, r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				// The header tells a client what to present, which is the
				// difference between "log in" and "something is broken".
				w.Header().Set("WWW-Authenticate", `Bearer realm="hecate"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		namespace := r.PathValue("namespace")
		if err := s.Auth.Authorize(ctx, subject, action, namespace); err != nil {
			var forbidden *Forbidden
			if errors.As(err, &forbidden) {
				writeError(w, http.StatusForbidden, forbidden.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		result, err := h(ctx, subject, r)
		if err != nil {
			writeOpsError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

// authenticated runs a handler for any authenticated caller, leaving
// authorisation to the handler.
//
// Exists for the handful of routes whose Action guard() cannot know: the
// settings writes, which check resources outside the path namespace, and the
// two Flux writes, whose resource is whatever the Gate's watch resolves to. A
// handler reached this way is responsible for deciding what the subject may
// do, which is why it takes the Subject and why every user of it carries a
// comment saying what it checks instead — this is not a general invitation to
// skip authorisation.
func (s *Server) authenticated(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		subject, err := s.Auth.Authenticate(ctx, r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hecate"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		result, err := h(ctx, subject, r)
		if err != nil {
			writeOpsError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

// ---------------------------------------------------------------- reads ----

// listNamespaces answers "where can I look?", which is what a namespace picker
// needs and what a text box cannot tell anyone.
//
// Discovery runs with the server's credentials, so every candidate is then
// checked against the caller's. Returning a namespace someone cannot read would
// be a directory of other teams' namespaces, which is a small information leak
// and a guaranteed support question when clicking it 403s.
func (s *Server) listNamespaces(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	visible, err := s.visibleNamespaces(ctx, subject)
	if err != nil {
		return nil, err
	}
	// Never null: the UI renders this straight into a list, and a null there is
	// a crash rather than an empty picker.
	return map[string]any{"namespaces": visible}, nil
}

// visibleNamespaces is every namespace with Hecate resources that this subject
// may read.
//
// Filtering, not refusing. Every other read authorises one namespace named in
// the path and answers 403 when the caller may not have it; these two routes
// span the cluster, and refusing the whole answer because one namespace is out
// of reach would make a cluster-wide view useless to exactly the team-scoped
// operators it is most useful to.
//
// Shared by the namespace picker and the overview deliberately: two callers
// deciding "what may this person see" separately is two chances to disagree,
// and the one that disagreed generously would be the bug nobody notices.
func (s *Server) visibleNamespaces(ctx context.Context, subject Subject) ([]string, error) {
	all, err := s.Ops.Namespaces(ctx)
	if err != nil {
		return nil, err
	}

	visible := make([]string, 0, len(all))
	for _, ns := range all {
		if err := s.Auth.Authorize(ctx, subject, ActionRead, ns); err != nil {
			var forbidden *Forbidden
			if errors.As(err, &forbidden) {
				continue
			}
			// A broken authorisation check is not the same as a refusal, and
			// silently dropping namespaces because the API server is unwell
			// would present as "my namespace vanished".
			return nil, err
		}
		visible = append(visible, ns)
	}
	return visible, nil
}

// overview is every Gate this caller can see, across every namespace.
//
// Not behind guard() for the same reason listNamespaces is not: guard
// authorises against a namespace in the path, and this route has none, so it
// would ask "may you read gates cluster-wide?" — a right a team-scoped operator
// has no reason to hold, and refusing them a board of their own Gates is
// precisely backwards.
func (s *Server) overview(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	visible, err := s.visibleNamespaces(ctx, subject)
	if err != nil {
		return nil, err
	}
	return s.Ops.Overview(ctx, visible)
}

func (s *Server) listBeacons(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Beacons(ctx, r.PathValue("namespace"))
}

func (s *Server) getBeacon(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Beacon(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

func (s *Server) listGates(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Gates(ctx, r.PathValue("namespace"))
}

func (s *Server) getGate(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Gate(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

func (s *Server) explainGate(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Explain(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

func (s *Server) listBundles(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Bundles(ctx, r.PathValue("namespace"))
}

func (s *Server) getBundle(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Bundle(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

// pollBeacon asks a Beacon to look at its sources now.
//
// The inbound webhook endpoint (#102): a git host or registry calls this after
// a push, and discovery drops from the poll interval to seconds. Authenticated
// like everything else here, by asking Kubernetes to review the bearer token,
// so a cluster that trusts a CI provider's OIDC issuer accepts that provider's
// workload token with nothing added and no shared secret to leak.
func (s *Server) pollBeacon(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	token, err := s.Ops.Poll(ctx, r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	// Echoed back so a caller can match it against status.lastHandledReconcileAt
	// and know its own request was the one that landed.
	return map[string]string{"requestedAt": token}, nil
}

// bundleEvidence answers "why was this allowed into production, and who
// allowed it?" — read-only, because it only reports what Fides already holds.
func (s *Server) bundleEvidence(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Evidence(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

func (s *Server) auditTrail(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Audit(ctx, r.PathValue("namespace"))
}

func (s *Server) listPassages(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	query := r.URL.Query()
	return s.Ops.Passages(ctx, r.PathValue("namespace"), query.Get("gate"), query.Get("bundle"))
}

func (s *Server) getPassage(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Passage(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

// --------------------------------------------------------------- writes ----

func (s *Server) promote(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	var body struct {
		Bundle string `json:"bundle"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Bundle == "" {
		return nil, &ops.RefusedError{Action: "promote", Reason: "no bundle given"}
	}
	// The actor is the authenticated caller, never anything the request body
	// says: a promotion is a compliance record, and a self-declared identity is
	// not one.
	return s.Ops.Promote(ctx, r.PathValue("namespace"), r.PathValue("name"), body.Bundle, subject.Name)
}

func (s *Server) approve(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	var body struct {
		Gate string `json:"gate"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Gate == "" {
		return nil, &ops.RefusedError{Action: "approve", Reason: "no gate given — an approval is for one Gate"}
	}
	namespace, bundle := r.PathValue("namespace"), r.PathValue("name")
	if err := s.Ops.Approve(ctx, namespace, bundle, body.Gate, subject.Name); err != nil {
		return nil, err
	}
	return map[string]any{"bundle": bundle, "gate": body.Gate, "approvedBy": subject.Name}, nil
}

func (s *Server) abort(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	namespace, passage := r.PathValue("namespace"), r.PathValue("name")
	if err := s.Ops.Abort(ctx, namespace, passage, subject.Name); err != nil {
		return nil, err
	}
	return map[string]any{"passage": passage, "aborted": true, "abortedBy": subject.Name}, nil
}

// ---------------------------------------------------------------- wiring ----

// decode reads a JSON body, tolerating an empty one.
func decode(r *http.Request, into any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	// Strict, for the same reason step configuration is: a misspelt field that
	// is silently dropped produces a confusing failure later rather than a
	// clear one now.
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return &ops.RefusedError{Action: "request", Reason: fmt.Sprintf("invalid body: %s", err)}
	}
	return nil
}

// writeOpsError maps an operations failure onto a status code.
//
// A refusal is not a malfunction: "this Bundle has not cleared staging" is an
// answer, and a client should be able to tell it from a server that broke.
// BadRequest is a request the caller can fix by sending different input.
//
// Distinct from ops.IsRefused, which means the request was fine and the state
// of the system said no. Conflating them tells someone to change their input
// when the input was never the problem.
type BadRequest struct{ Reason string }

func (e *BadRequest) Error() string { return e.Reason }

func writeOpsError(w http.ResponseWriter, err error) {
	var badRequest *BadRequest
	var forbidden *Forbidden
	var problems *stepProblemsError
	var exists *pathExistsError
	switch {
	case errors.As(err, &problems):
		// Structured, not just a message: the form that sent this needs to
		// point at which step is wrong, the same way a Gate's own admission
		// would (hecate#172 scope item 4).
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    problems.Error(),
			"status":   strings.ToLower(http.StatusText(http.StatusBadRequest)),
			"problems": problems.dto(),
		})
	case errors.As(err, &badRequest):
		writeError(w, http.StatusBadRequest, badRequest.Error())
	case errors.As(err, &forbidden):
		// Handlers that authorise for themselves — the settings writes, which
		// check a resource guard() knows nothing about — return this rather
		// than writing a response, so it has to be translated here. Without
		// this case a refusal is reported as a server fault, which sends
		// someone debugging Hecate when the answer is "you may not do that".
		writeError(w, http.StatusForbidden, forbidden.Error())
	case errors.As(err, &exists):
		// 409, the same as ops.IsRefused below: the request was well-formed,
		// and what refused it is the state of the target repository, not the
		// input. Retrying with overwrite:true — a different request — is the
		// right response, which is what 409 says.
		writeError(w, http.StatusConflict, exists.Error())
	case ops.IsNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	case ops.IsRefused(err):
		// 409 rather than 400: the request was well-formed, and the state of
		// the system is what refused it. Retrying after the state changes is
		// the right response, which is what 409 says.
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error":  message,
		"status": strings.ToLower(http.StatusText(status)),
	})
}

func (s *Server) preflight(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.Preflight(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

// ------------------------------------------------------------------ flux ----

func (s *Server) fluxResources(ctx context.Context, _ Subject, r *http.Request) (any, error) {
	return s.Ops.FluxResources(ctx, r.PathValue("namespace"), r.PathValue("name"))
}

// authorizeFluxWrite resolves what a Flux write would actually patch, and
// checks the caller against that — not against a fixed guess.
//
// Resolution has to come first, which is why these two routes cannot use
// guard(): a Gate's flux check may override the apiVersion, so the group and
// resource being written are a property of the Gate, not of the route. Asking
// for `patch kustomizations` regardless would mean a caller holding only that
// right could suspend a HelmRelease — a different API group — through Hecate,
// because Hecate patches with its own ServiceAccount and the API server never
// sees the caller.
//
// Reading the Gate before the permission check is deliberate and is the price:
// an authenticated caller can learn whether a Gate they named exists. The Gate
// is read with Hecate's own credentials either way, nothing is written, and
// the alternative is authorising against a resource that is not the one about
// to be patched.
func (s *Server) authorizeFluxWrite(
	ctx context.Context, subject Subject, namespace, gate, kind, name string,
) (health.FluxResource, error) {
	ref, gvk, err := s.Ops.FluxTarget(ctx, namespace, gate, kind, name)
	if err != nil {
		return health.FluxResource{}, err
	}
	if err := s.Auth.Authorize(ctx, subject, ActionOperateFlux(gvk), namespace); err != nil {
		return health.FluxResource{}, err
	}
	return ref, nil
}

// suspendFlux stops or restarts Flux reconciling one resource.
//
// The suspending is the dangerous half and the resuming is the remedy, so both
// live on one route: an operator who can stop reconciliation must always be
// able to start it again, and splitting them invites a deployment where the
// second permission was never granted.
func (s *Server) suspendFlux(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	var body struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Suspend bool   `json:"suspend"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Kind == "" || body.Name == "" {
		return nil, &BadRequest{Reason: "kind and name are required"}
	}
	namespace, gate := r.PathValue("namespace"), r.PathValue("name")
	ref, err := s.authorizeFluxWrite(ctx, subject, namespace, gate, body.Kind, body.Name)
	if err != nil {
		return nil, err
	}
	if err := s.Ops.SetFluxSuspend(ctx, namespace, ref, body.Suspend); err != nil {
		return nil, err
	}
	// The actor is echoed because a suspension outlives the session that made
	// it, and "who stopped this" is the first question asked about one.
	return map[string]any{
		"kind": body.Kind, "name": body.Name,
		"suspended": body.Suspend, "by": subject.Name,
	}, nil
}

func (s *Server) reconcileFlux(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Kind == "" || body.Name == "" {
		return nil, &BadRequest{Reason: "kind and name are required"}
	}
	namespace := r.PathValue("namespace")
	ref, err := s.authorizeFluxWrite(ctx, subject, namespace, r.PathValue("name"),
		body.Kind, body.Name)
	if err != nil {
		return nil, err
	}
	stamp, err := s.Ops.ReconcileFlux(ctx, namespace, ref)
	if err != nil {
		return nil, err
	}
	// Echoed so a caller can match it against status.lastHandledReconcileAt and
	// know its own request landed.
	return map[string]any{"requestedAt": stamp}, nil
}
