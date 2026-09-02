package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/olafkfreund/hecate/pkg/passage"
)

// validatePassageRequest is the step list a form is trying out — the same
// composedStep shape authorPassage takes, minus everything about where to
// open a pull request, because this checks nothing that needs one.
type validatePassageRequest struct {
	Steps []composedStep `json:"steps"`
}

// validatePassage reports what Registry.Validate finds wrong with a step
// list, without opening a pull request or refusing anything itself
// (hecate#172 scope item 4, docs/DECISIONS.md D60).
//
// Always 200, problems or not: this is feedback for a form still being
// edited, not a gate. Reuses passage.Registry.Validate and
// stepProblemsError.dto() rather than restating the rules — authorPassage
// calls the exact same two before it will open a pull request, so the
// browser and the admission path refuse the same things for the same
// reasons by construction, not by two implementations kept in sync by hand.
// A client that skips this endpoint is still refused there.
//
// Not namespaced, like stepSchemas: Validate reads no cluster state, only
// the controller's own step catalogue, so the answer for a given step list
// is the same in every namespace and there is nothing here for guard() to
// authorise against.
func (s *Server) validatePassage(_ context.Context, _ Subject, r *http.Request) (any, error) {
	var req validatePassageRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
	}

	var problems []passage.StepProblem
	if s.Steps != nil {
		problems = s.Steps.Validate(toSteps(req.Steps))
	}
	return map[string]any{"problems": (&stepProblemsError{Problems: problems}).dto()}, nil
}
