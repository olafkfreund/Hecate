package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const validatePath = "/api/v1alpha1/passages/validate"

// TestValidatePassageNeedsACredential: unauthenticated the same as every
// other route (steps_test.go's TestStepSchemasNeedACredential is the same
// check for the other unnamespaced, read-only route).
func TestValidatePassageNeedsACredential(t *testing.T) {
	s, _ := newServer(t, map[string]string{"t": "alice"}, nil)
	s.Steps = realRegistry(t)

	rec := call(t, s, "", http.MethodPost, validatePath, `{"steps":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestValidatePassageIsUnauthenticatedReadOnly asserts the endpoint asks for
// nothing beyond a credential: a caller holding no Hecate grant at all still
// gets an answer, because this never touches the cluster and never opens
// anything.
func TestValidatePassageNeedsNoGrant(t *testing.T) {
	s, log := newServer(t, map[string]string{"t": "nobody@example.com"}, grants{})
	s.Steps = realRegistry(t)

	rec := call(t, s, "t", http.MethodPost, validatePath, `{"steps":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(log.reviews) != 0 {
		t.Errorf("made %d SubjectAccessReviews, want 0 — this route only authenticates", len(log.reviews))
	}
}

func decodeProblems(t *testing.T, rec interface{ Bytes() []byte }) []struct {
	Index   int    `json:"index"`
	Uses    string `json:"uses"`
	Message string `json:"message"`
} {
	t.Helper()
	var resp struct {
		Problems []struct {
			Index   int    `json:"index"`
			Uses    string `json:"uses"`
			Message string `json:"message"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp.Problems
}

// TestValidatePassageReportsAnEmptyStepList checks a valid list comes back
// with an empty (never null) problems array and a 200 — this is feedback for
// a form being edited, not a gate that refuses.
func TestValidatePassageAcceptsAValidStepList(t *testing.T) {
	s, _ := newServer(t, map[string]string{"t": "alice"}, grants{})
	s.Steps = realRegistry(t)

	rec := call(t, s, "t", http.MethodPost, validatePath, `{"steps":`+validSteps+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if problems := decodeProblems(t, rec.Body); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none for a valid step list", problems)
	}
	// Never null: the same reason overview and namespaces return [] rather
	// than omitting the field — a form binding straight to this would crash
	// mapping over null.
	if !strings.Contains(rec.Body.String(), `"problems":[]`) {
		t.Errorf("body = %s, want an explicit empty array, not a null or omitted field", rec.Body.String())
	}
}

// TestValidatePassageMatchesRegistryValidate is the "same reasons" guarantee
// the issue asks for: whatever passage.Registry.Validate reports for a step
// list, the endpoint must report byte-for-byte the same index/uses/message —
// not a handler-side reimplementation or special case of any rule.
func TestValidatePassageMatchesRegistryValidate(t *testing.T) {
	s, _ := newServer(t, map[string]string{"t": "alice"}, grants{})
	reg := realRegistry(t)
	s.Steps = reg

	body := `{"steps":[` +
		`{"uses":"git-commit","with":{}},` + // missing message: CheckConfig fails
		`{"uses":"no-such-step"},` + // unknown uses
		`{"as":"dup"},` + // no uses at all
		`{"uses":"git-clone","as":"dup","with":{"repo":"x"}}` + // duplicate alias
		`]}`

	rec := call(t, s, "t", http.MethodPost, validatePath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeProblems(t, rec.Body)

	var req validatePassageRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	want := reg.Validate(toSteps(req.Steps))

	if len(got) != len(want) {
		t.Fatalf("endpoint reported %d problems, Registry.Validate reported %d directly:\nendpoint: %+v\ndirect:   %+v",
			len(got), len(want), got, want)
	}
	for i, p := range want {
		if got[i].Index != p.Index || got[i].Uses != p.Uses || got[i].Message != p.Err.Error() {
			t.Errorf("problem %d = %+v, want index=%d uses=%q message=%q (from Registry.Validate directly)",
				i, got[i], p.Index, p.Uses, p.Err.Error())
		}
	}
}

// TestValidatePassageNeverOpensAnything: the endpoint must never touch a
// provider or git — it is read-only feedback, not a lighter-weight author.
func TestValidatePassageNeverTouchesProviderOrGit(t *testing.T) {
	host := &fakeAuthorHost{}
	s, published := authorServer(t, grants{}, host)

	rec := call(t, s, "t", http.MethodPost, validatePath, `{"steps":`+validSteps+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(host.opened) != 0 {
		t.Error("validate must never open a pull request")
	}
	if len(*published) != 0 {
		t.Error("validate must never write to git")
	}
}
