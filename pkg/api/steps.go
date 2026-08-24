package api

import (
	"context"
	"net/http"

	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

// stepSchemas serves a JSON Schema per step, so a form can be generated from
// the same structs the steps validate against (#114).
//
// Not namespaced and not cluster-backed: the answer is the same everywhere,
// because it describes the controller's own step catalogue rather than anything
// in a cluster. Authenticated all the same — every other route is, and a route
// that is the exception for no reason is one nobody remembers is the exception.
func (s *Server) stepSchemas(_ context.Context, _ Subject, _ *http.Request) (any, error) {
	return steps.Schemas()
}
