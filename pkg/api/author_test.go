package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/passage/steps"
	"github.com/olafkfreund/hecate/pkg/provider"
)

/**
 * authorPassage never writes to the cluster — it opens a pull request through
 * pkg/provider (hecate#172 stage 2). These tests fake the provider, the same
 * way pkg/passage/steps/pullrequest_test.go does, and fake the git
 * clone/commit/push half behind the `publish` seam so nothing here touches a
 * real remote. The step Registry is the real one, built from steps.All the
 * same way cmd/hecate-api/main.go builds it — so "refuses an invalid step
 * list" exercises the actual admission rules, not a stand-in for them.
 */

const authorPath = "/api/v1alpha1/namespaces/acme/passages/author"

// fakeAuthorHost stands in for a git host's API.
type fakeAuthorHost struct {
	pr      *provider.PullRequest
	opened  []provider.PullRequestSpec
	openErr error
}

func (f *fakeAuthorHost) Kind() provider.Kind { return provider.GitHub }

func (f *fakeAuthorHost) EnsurePullRequest(
	_ context.Context, spec provider.PullRequestSpec,
) (*provider.PullRequest, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened = append(f.opened, spec)
	pr := f.pr
	if pr == nil {
		pr = &provider.PullRequest{Number: 9, URL: "https://github.test/acme/fleet/pull/9", State: provider.Open}
	}
	pr.Head = spec.Head
	return pr, nil
}

func (f *fakeAuthorHost) PullRequest(context.Context, provider.Repo, int) (*provider.PullRequest, error) {
	return f.pr, nil
}

func (f *fakeAuthorHost) SetCommitStatus(context.Context, provider.CommitStatus) error { return nil }

// forgeSecret is the credentialsRef every test below points at.
func forgeSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "forge", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("s3cret")},
	}
}

// realRegistry is the same step catalogue the controller and cmd/hecate-api
// register — built once so every test validates against the real admission
// rules rather than a stand-in for them.
func realRegistry(t *testing.T) *passage.Registry {
	t.Helper()
	reg := passage.NewRegistry()
	for _, r := range steps.All(steps.Deps{}) {
		reg.MustRegister(r)
	}
	return reg
}

// authorServer wires a Server the way cmd/hecate-api/main.go does, plus the
// two test seams: a fake provider and a fake publish that records what it was
// asked to commit instead of touching a real git remote.
func authorServer(t *testing.T, rbac grants, host *fakeAuthorHost) (*Server, *[]publishRequest) {
	t.Helper()
	s, _ := newServer(t,
		map[string]string{"t": "author@example.com"},
		rbac,
		forgeSecret(),
	)
	s.Steps = realRegistry(t)
	s.providers = func(provider.Kind, provider.Config) (provider.Provider, error) { return host, nil }

	var published []publishRequest
	s.publish = func(_ context.Context, req publishRequest) (string, error) {
		published = append(published, req)
		return "main", nil
	}
	return s, &published
}

const validSteps = `[{"uses":"git-commit","with":{"message":"promote"}}]`

func validBody() string {
	return `{"steps":` + validSteps + `,"repo":"https://github.com/acme/fleet.git",` +
		`"path":"demo/pipeline.yaml","credentialsRef":"forge"}`
}

// TestAuthorPassageRequiresAuthorization is the auth mutation-check: a caller
// who does not hold "create gates" must be refused, and neither the git side
// nor the provider must ever be touched for them.
func TestAuthorPassageRequiresAuthorization(t *testing.T) {
	host := &fakeAuthorHost{}
	s, published := authorServer(t, grants{"author@example.com": {"list gates": true}}, host)

	rec := call(t, s, "t", http.MethodPost, authorPath, validBody())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — the caller holds no right to author a Passage", rec.Code)
	}
	if len(host.opened) != 0 {
		t.Error("a refused caller must never reach the provider")
	}
	if len(*published) != 0 {
		t.Error("a refused caller must never reach git")
	}
}

// TestAuthorPassageChecksTheRightPermission asks what guard() asked for, not
// only whether it refused — authorising the wrong resource would pass this
// suite while granting the wrong people the right.
func TestAuthorPassageChecksTheRightPermission(t *testing.T) {
	host := &fakeAuthorHost{}
	s, log := newServer(t,
		map[string]string{"t": "author@example.com"},
		grants{"author@example.com": {"create gates": true}},
		forgeSecret(),
	)
	s.Steps = realRegistry(t)
	s.providers = func(provider.Kind, provider.Config) (provider.Provider, error) { return host, nil }
	s.publish = func(context.Context, publishRequest) (string, error) { return "main", nil }

	rec := call(t, s, "t", http.MethodPost, authorPath, validBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	last := log.last()
	if last.ResourceAttributes.Resource != "gates" || last.ResourceAttributes.Verb != "create" ||
		last.ResourceAttributes.Group != "hecate.dev" {
		t.Fatalf("authorised %s %s.%s, want create gates.hecate.dev",
			last.ResourceAttributes.Verb, last.ResourceAttributes.Resource, last.ResourceAttributes.Group)
	}
}

// TestAuthorPassageRefusesInvalidSteps is the validation mutation-check: a
// step list that would not admit must never reach git or the provider.
func TestAuthorPassageRefusesInvalidSteps(t *testing.T) {
	host := &fakeAuthorHost{}
	s, published := authorServer(t, grants{"author@example.com": {"create gates": true}}, host)

	body := `{"steps":[{"uses":"git-commit","with":{}}],"repo":"https://github.com/acme/fleet.git",` +
		`"path":"demo/pipeline.yaml","credentialsRef":"forge"}`
	rec := call(t, s, "t", http.MethodPost, authorPath, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 — git-commit with no message would not admit", rec.Code)
	}
	var resp struct {
		Problems []struct {
			Index   int    `json:"index"`
			Uses    string `json:"uses"`
			Message string `json:"message"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Problems) != 1 || resp.Problems[0].Uses != "git-commit" ||
		!strings.Contains(resp.Problems[0].Message, "message is required") {
		t.Fatalf("problems = %+v, want one problem naming git-commit's missing message", resp.Problems)
	}
	if len(host.opened) != 0 {
		t.Error("an invalid step list must never reach the provider")
	}
	if len(*published) != 0 {
		t.Error("an invalid step list must never reach git")
	}
}

// TestAuthorPassageOpensAPullRequest is the happy path: a valid step list is
// rendered, committed and opened as a pull request with the right target.
func TestAuthorPassageOpensAPullRequest(t *testing.T) {
	host := &fakeAuthorHost{}
	s, published := authorServer(t, grants{"author@example.com": {"create gates": true}}, host)

	rec := call(t, s, "t", http.MethodPost, authorPath, validBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Branch string `json:"branch"`
		Repo   string `json:"repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Number != 9 || resp.URL == "" || resp.State != string(provider.Open) {
		t.Fatalf("response = %+v, did not carry back what the provider returned", resp)
	}

	if len(host.opened) != 1 {
		t.Fatalf("opened %d pull requests, want 1", len(host.opened))
	}
	spec := host.opened[0]
	if spec.Repo.Host != "github.com" || spec.Repo.Owner != "acme" || spec.Repo.Name != "fleet" {
		t.Errorf("opened against %s, want github.com/acme/fleet", spec.Repo)
	}
	if spec.Base != "main" {
		t.Errorf("base = %q, want the branch publish reported", spec.Base)
	}
	if spec.Head != "hecate/author-pipeline" {
		t.Errorf("head = %q, want a name derived from path", spec.Head)
	}

	if len(*published) != 1 {
		t.Fatalf("published %d times, want 1", len(*published))
	}
	pub := (*published)[0]
	if pub.CloneURL != "https://github.com/acme/fleet.git" || pub.Path != "demo/pipeline.yaml" {
		t.Errorf("published %+v, want the request's repo and path", pub)
	}
	if !strings.Contains(string(pub.Content), "uses: git-commit") ||
		!strings.Contains(string(pub.Content), "message: promote") {
		t.Errorf("rendered YAML = %q, missing the composed step", pub.Content)
	}
}

// TestAuthorPassageValidatesTheRequest is table-driven over the fields that
// have no reasonable default, in the idiom of the other write-endpoint tests
// in this package.
func TestAuthorPassageValidatesTheRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no steps", `{"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"empty steps", `{"steps":[],"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"no repo", `{"steps":` + validSteps + `,"path":"a.yaml","credentialsRef":"forge"}`},
		{"no path", `{"steps":` + validSteps + `,"repo":"https://github.com/acme/fleet.git","credentialsRef":"forge"}`},
		{"no credentialsRef", `{"steps":` + validSteps + `,"repo":"https://github.com/acme/fleet.git","path":"a.yaml"}`},
		{"unroutable repo", `{"steps":` + validSteps + `,"repo":"not a url","path":"a.yaml","credentialsRef":"forge"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeAuthorHost{}
			s, published := authorServer(t, grants{"author@example.com": {"create gates": true}}, host)

			rec := call(t, s, "t", http.MethodPost, authorPath, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(host.opened) != 0 || len(*published) != 0 {
				t.Error("an invalid request must never reach git or the provider")
			}
		})
	}
}
