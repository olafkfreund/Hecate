//go:build e2e

package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/provider"
)

/**
 * The authoring path against real GitHub (hecate#172 scope item 3).
 *
 * pkg/api/author_test.go proves everything up to the two seams and stops
 * there: `publish` and `providers` are both replaced with fakes, so the
 * request handling, the RBAC, the validation refusal and the rendered
 * manifest are all covered, and the halves that actually reach a git host —
 * gitPublish's clone/write/commit/push, and provider.New's pull request — are
 * covered against nothing. That is the same gap #118 left on the GitHub App:
 * correct up to the wire, unobserved past it.
 *
 * This is in package api rather than test/provider because both seams are
 * unexported and there is nothing to be gained from exporting them for a
 * test: the fields exist precisely so production wiring cannot reach past
 * them. Leaving them nil is what makes this test the real thing.
 *
 * The Kubernetes client is still the fake one — the cluster is not what is
 * unproven here, and the only thing this endpoint reads from it is a Secret.
 * GitHub is what is unproven, so GitHub is real.
 *
 * Skipped without HECATE_E2E_GITHUB_TOKEN, so the suite stays runnable by
 * anyone without credentials to a third party.
 */

// The same throwaway fleet repository test/provider writes to. Nothing in it
// is deployed.
const (
	fleetOwner = "olafkfreund"
	fleetName  = "hecate-e2e-fleet"
)

// TestAuthorPassageOpensARealPullRequest drives POST .../passages/author with
// both seams left at their production defaults, then reads the result back
// out of GitHub rather than out of a fake's recorded call.
//
// The assertion that matters is the last one: the file that actually landed
// on the branch decodes as a whole Passage and its step list passes the same
// Registry.Validate admission would run. A manifest that renders correctly in
// memory and arrives truncated, re-encoded or with its `with:` block mangled
// by the transport would pass every test in author_test.go.
func TestAuthorPassageOpensARealPullRequest(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("HECATE_E2E_GITHUB_TOKEN"))
	if token == "" {
		t.Skip("no HECATE_E2E_GITHUB_TOKEN; skipping the GitHub authoring e2e")
	}

	repo := provider.Repo{Host: "github.com", Owner: fleetOwner, Name: fleetName}

	// Unique per run, so two runs never collide on a path or a branch and a
	// failed run leaves its evidence behind rather than being overwritten by
	// the next one. Digits and hyphens only, so slug() leaves the basename
	// alone and the derived branch below is predictable.
	stamp := time.Now().UnixNano()
	path := fmt.Sprintf("authored/e2e-%d.yaml", stamp)
	// Head is deliberately not sent: deriving it from the path is production
	// behaviour, and this is the only place it runs against a host that has
	// to accept the name.
	wantBranch := fmt.Sprintf("hecate/author-e2e-%d", stamp)
	t.Cleanup(func() { closeBranch(t, token, repo, wantBranch) })

	s, _ := newServer(t,
		map[string]string{"t": "author@example.com"},
		grants{"author@example.com": {"create gates/author": true}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "forge", Namespace: "acme"},
			Data:       map[string][]byte{"token": []byte(token)},
		},
	)
	s.Steps = realRegistry(t)
	// s.publish and s.providers are left nil on purpose: nil is gitPublish and
	// provider.New. That is the whole point of this file.

	body, err := json.Marshal(AuthorPassageRequest{
		Name: fmt.Sprintf("e2e-%d", stamp), Gate: "staging", Bundle: "b1",
		Steps: []composedStep{{
			Uses: "git-commit",
			With: json.RawMessage(`{"message":"promote from the authoring e2e"}`),
		}},
		Repo:           fmt.Sprintf("https://github.com/%s.git", repo.Slug()),
		Path:           path,
		Base:           "main",
		Title:          "e2e: author a Passage",
		Body:           "Opened by Hecate's authoring end-to-end test. Safe to close.",
		CredentialsRef: "forge",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := call(t, s, "t", http.MethodPost, authorPath, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got AuthoredPullRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Number == 0 || got.URL == "" {
		t.Fatalf("response = %+v, want a real pull request number and URL", got)
	}
	if got.State != string(provider.Open) {
		t.Errorf("state = %q, want %q", got.State, provider.Open)
	}
	if got.Branch != wantBranch {
		t.Errorf("branch = %q, want %q derived from the path", got.Branch, wantBranch)
	}
	t.Logf("opened %s", got.URL)

	// GitHub's own view of the pull request, not the adapter's. A response
	// assembled from the request rather than from what the host accepted would
	// agree with itself above and disagree here.
	var live struct {
		State string `json:"state"`
		Head  struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	fleetAPI(t, token, http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls/%d", repo.Slug(), got.Number), nil, &live)
	if live.State != "open" || live.Head.Ref != wantBranch || live.Base.Ref != "main" {
		t.Errorf("GitHub has %s %s -> %s, want an open %s -> main",
			live.State, live.Head.Ref, live.Base.Ref, wantBranch)
	}

	// The round trip that only a real host can prove: read the file back off
	// the branch and check it is still a whole, admissible Passage.
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	fleetAPI(t, token, http.MethodGet,
		fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo.Slug(), path, wantBranch), nil, &content)
	if content.Encoding != "base64" {
		t.Fatalf("GitHub returned %q content, cannot read the committed file", content.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		t.Fatalf("decoding the committed file: %v", err)
	}

	var manifest v1alpha1.Passage
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the committed file does not decode as a Passage: %v\n%s", err, raw)
	}
	if manifest.Kind != "Passage" || manifest.APIVersion != v1alpha1.GroupVersion.String() {
		t.Errorf("TypeMeta = %s/%s, want a Passage the cluster can apply",
			manifest.APIVersion, manifest.Kind)
	}
	if manifest.Spec.Gate != "staging" || manifest.Spec.Bundle != "b1" {
		t.Errorf("spec.gate/bundle = %s/%s, want staging/b1",
			manifest.Spec.Gate, manifest.Spec.Bundle)
	}
	// The claim this whole endpoint rests on: what a human merges admits. The
	// in-memory check happens before the commit, so it says nothing about what
	// survived the push.
	if problems := s.Steps.Validate(manifest.Spec.Steps); len(problems) > 0 {
		t.Errorf("the step list that reached GitHub would not admit: %v\n%s", problems, raw)
	}
}

// closeBranch tidies up: close the pull request, then delete the branch.
//
// Best-effort — a leftover branch in a throwaway repo is untidy, whereas
// failing the test over it would report a working authoring path as broken.
// Logged rather than discarded, though: cleanup that has quietly stopped
// working leaves branches piling up in the fleet repo with nothing anywhere
// saying so, and the next person to look assumes the test is leaking.
func closeBranch(t *testing.T, token string, repo provider.Repo, branch string) {
	t.Helper()
	tidy := func(method, path string, body any, out any) {
		if err := fleetRequest(t, token, method, path, body, out); err != nil {
			t.Logf("cleanup: %s %s: %v", method, path, err)
		}
	}

	var open []struct {
		Number int `json:"number"`
	}
	tidy(http.MethodGet, fmt.Sprintf(
		"/repos/%s/pulls?state=open&head=%s:%s", repo.Slug(), repo.Owner, branch), nil, &open)
	for _, pr := range open {
		tidy(http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo.Slug(), pr.Number),
			map[string]any{"state": "closed"}, nil)
	}
	tidy(http.MethodDelete,
		fmt.Sprintf("/repos/%s/git/refs/heads/%s", repo.Slug(), branch), nil, nil)
}

// fleetAPI calls GitHub and fails the test if it refuses.
func fleetAPI(t *testing.T, token, method, path string, body, out any) {
	t.Helper()
	if err := fleetRequest(t, token, method, path, body, out); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
}

// fleetRequest is the same call without the failure, for cleanup.
func fleetRequest(t *testing.T, token, method, path string, body, out any) error {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%d: %s", resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
