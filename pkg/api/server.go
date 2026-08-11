package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// Server is Hecate's HTTP API.
type Server struct {
	Ops  *ops.Ops
	Auth *Authenticator
	// Version is reported at /healthz, so an operator can tell which build is
	// answering without reading the deployment.
	Version string
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

	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/beacons",
		s.guard(ActionRead, s.listBeacons))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/beacons/{name}",
		s.guard(ActionRead, s.getBeacon))
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
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/passages",
		s.guard(ActionRead, s.listPassages))
	mux.Handle("GET /api/v1alpha1/namespaces/{namespace}/passages/{name}",
		s.guard(ActionRead, s.getPassage))

	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/gates/{name}/promote",
		s.guard(ActionPromote, s.promote))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/bundles/{name}/approve",
		s.guard(ActionApprove, s.approve))
	mux.Handle("POST /api/v1alpha1/namespaces/{namespace}/passages/{name}/abort",
		s.guard(ActionAbort, s.abort))

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

// ---------------------------------------------------------------- reads ----

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
func writeOpsError(w http.ResponseWriter, err error) {
	switch {
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
