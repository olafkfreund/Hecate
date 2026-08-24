package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

// The route a form generator reads. Its value is entirely in what it carries,
// so a test asserting only a 200 would pass on an empty object.
func TestStepSchemasAreServed(t *testing.T) {
	s, _ := newServer(t, map[string]string{"t": "alice"}, nil)

	rec := call(t, s, "t", "GET", "/api/v1alpha1/steps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %s", err)
	}

	want, err := steps.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("served %d schemas, the catalogue has %d", len(got), len(want))
	}

	// One step checked through: a schema with no properties is a form with no
	// fields, which the count above would not notice.
	git, ok := got["git-clone"]
	if !ok {
		t.Fatal("no schema for git-clone")
	}
	repo, ok := git.Properties["repo"]
	if !ok {
		t.Fatal("git-clone offers no repo field")
	}
	if repo.Type != "string" {
		t.Errorf("repo type = %q, want string", repo.Type)
	}
	// The descriptions are the reason this is generated at build time rather
	// than reflected at runtime. Serving them empty would defeat the point
	// silently — the form would render, with no help text.
	if repo.Description == "" {
		t.Error("repo has no description — the doc comments did not reach the schema")
	}
}

// Nothing here is secret, but every other route is authenticated and an
// exception nobody remembers is an exception that grows.
func TestStepSchemasNeedACredential(t *testing.T) {
	s, _ := newServer(t, map[string]string{"t": "alice"}, nil)

	if rec := call(t, s, "", "GET", "/api/v1alpha1/steps", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
