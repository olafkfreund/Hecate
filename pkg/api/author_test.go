package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/olafkfreund/hecate/api/v1alpha1"
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
	return `{"name":"p1","gate":"staging","bundle":"b1","steps":` + validSteps +
		`,"repo":"https://github.com/acme/fleet.git","path":"demo/pipeline.yaml","credentialsRef":"forge"}`
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

	body := `{"name":"p1","gate":"staging","bundle":"b1","steps":[{"uses":"git-commit","with":{}}],` +
		`"repo":"https://github.com/acme/fleet.git","path":"demo/pipeline.yaml","credentialsRef":"forge"}`
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

	// The round trip: what gets committed must be a whole, applyable Passage —
	// not a fragment. Unmarshalling it back into v1alpha1.Passage and checking
	// gate/bundle/steps survive is the point; a bare `steps:` block (what this
	// endpoint used to write) would fail this by decoding to an empty Spec.
	var manifest v1alpha1.Passage
	if err := yaml.Unmarshal(pub.Content, &manifest); err != nil {
		t.Fatalf("committed content does not decode as a Passage: %v\n%s", err, pub.Content)
	}
	if manifest.Kind != "Passage" || manifest.APIVersion != v1alpha1.GroupVersion.String() {
		t.Errorf("TypeMeta = %s/%s, want a Passage the cluster can apply", manifest.APIVersion, manifest.Kind)
	}
	if manifest.Name != "p1" || manifest.Namespace != "acme" {
		t.Errorf("metadata = %s/%s, want p1 in acme", manifest.Namespace, manifest.Name)
	}
	if manifest.Spec.Gate != "staging" || manifest.Spec.Bundle != "b1" {
		t.Errorf("spec.gate/bundle = %s/%s, want staging/b1", manifest.Spec.Gate, manifest.Spec.Bundle)
	}
	if len(manifest.Spec.Steps) != 1 || manifest.Spec.Steps[0].Uses != "git-commit" {
		t.Fatalf("spec.steps = %+v, want the composed git-commit step", manifest.Spec.Steps)
	}
	var with struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(manifest.Spec.Steps[0].With.Raw, &with); err != nil || with.Message != "promote" {
		t.Errorf("step With = %s, want message: promote to survive", manifest.Spec.Steps[0].With.Raw)
	}
}

// TestGitPublishRefusesAnExistingPath is the second mutation-checked guard: a
// Path that already holds something on the target branch must never be
// silently overwritten (D58). This exercises the real gitPublish against a
// local bare repository — no fake, since the fake `publish` seam used
// elsewhere in this file stands in for exactly the logic under test here.
func TestGitPublishRefusesAnExistingPath(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})

	_, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-test",
		Path:     "demo/pipeline.yaml",
		Content:  []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n"),
	})

	var exists *pathExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("got %v, want a pathExistsError — demo/pipeline.yaml already exists", err)
	}
}

// TestGitPublishOverwriteReplacesAnExistingPath is the escape hatch: setting
// Overwrite is what turns the refusal above into a normal commit.
func TestGitPublishOverwriteReplacesAnExistingPath(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})

	base, err := gitPublish(context.Background(), publishRequest{
		CloneURL:  origin,
		Head:      "hecate/author-test",
		Path:      "demo/pipeline.yaml",
		Content:   []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n"),
		Overwrite: true,
	})
	if err != nil {
		t.Fatalf("Overwrite:true should have let this through: %v", err)
	}
	if base == "" {
		t.Error("expected the base branch to be reported")
	}
}

// TestGitPublishAllowsANewPath is the control: a Path that does not exist yet
// must never be refused.
func TestGitPublishAllowsANewPath(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})

	if _, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-test",
		Path:     "demo/new-passage.yaml",
		Content:  []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n"),
	}); err != nil {
		t.Fatalf("a new path must not be refused: %v", err)
	}
}

// TestGitPublishWritesANestedNewPath is the pitfall case the containment
// rewrite (realAncestor's resolved-ancestor-plus-remainder) exists for: Path
// is two directories deep and *neither* directory exists yet, so
// realAncestor's resolved ancestor is the checkout root itself, several
// levels above the file. A version that dropped the not-yet-created
// "apps/dev" remainder and joined only the basename onto that root would
// return no error at all — it would just silently write "passage.yaml" at
// the repository root instead of "apps/dev/passage.yaml". So this checks the
// actual location, not only that gitPublish was happy: it clones the branch
// gitPublish pushed and reads the file back from the exact requested path.
func TestGitPublishWritesANestedNewPath(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})
	content := []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n")

	if _, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-nested",
		Path:     "apps/dev/passage.yaml",
		Content:  content,
	}); err != nil {
		t.Fatalf("a new, two-level-deep path must not be refused: %v", err)
	}

	verify := t.TempDir()
	if _, err := git.PlainClone(verify, false, &git.CloneOptions{
		URL:           origin,
		ReferenceName: plumbing.NewBranchReferenceName("hecate/author-nested"),
		SingleBranch:  true,
	}); err != nil {
		t.Fatalf("cloning the pushed branch to verify: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(verify, "apps", "dev", "passage.yaml"))
	if err != nil {
		t.Fatalf("the file did not land at apps/dev/passage.yaml (git staged the wrong path): %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content at apps/dev/passage.yaml = %q, want %q", got, content)
	}
	// The regression this guards: a rewrite that discarded the remainder
	// would place the file here instead.
	if _, err := os.Stat(filepath.Join(verify, "passage.yaml")); !os.IsNotExist(err) {
		t.Error("the file landed at the repository root instead of the requested nested path")
	}
}

// TestGitPublishRefusesTraversal is the lexical case filepath.Rel already
// caught before this change — confirmed still refused.
func TestGitPublishRefusesTraversal(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})

	_, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-test",
		Path:     "../../etc/passwd",
		Content:  []byte("x"),
	})
	if err == nil {
		t.Fatal("expected a refusal — ../../etc/passwd escapes the checkout")
	}
}

// TestGitPublishRefusesAnAbsolutePath is the other lexical case: an absolute
// Path is never "relative to the repository", so it is refused outright
// rather than silently rebased under the checkout root.
func TestGitPublishRefusesAnAbsolutePath(t *testing.T) {
	origin := originAuthorRepo(t, map[string]string{"demo/pipeline.yaml": "kind: Gate\n"})

	_, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-test",
		Path:     "/etc/passwd",
		Content:  []byte("x"),
	})
	if err == nil {
		t.Fatal("expected a refusal — an absolute path is never relative to the repository")
	}
}

// escapeTarget is a symlink target relative enough to reach outside no
// matter how deeply nested gitPublish's own scratch clone happens to be —
// deliberately more ".." components than any real checkout is nested.
//
// Not an absolute path: go-git's own worktree checkout (helper/chroot)
// rewrites an *absolute* symlink target to live under the checkout root
// before creating it — its own, unrelated defence against exactly this class
// of escape — so a fixture using one would pass without ever exercising the
// guard this test is for. A relative target is passed straight through
// untouched, and the OS resolves it exactly as written, which is the shape
// that actually needs Hecate's own check.
func escapeTarget(outside string) string {
	return strings.Repeat("../", 12) + strings.TrimPrefix(filepath.ToSlash(outside), "/")
}

// TestGitPublishRefusesASymlinkEscape is the case filepath.Rel cannot catch,
// because it is purely lexical: a symlink committed into the checkout — which
// the same HTTP caller controls, via `repo` — can point a path component
// outside the checkout entirely. `apps -> ../../../…/outside` here stands in
// for a fleet repo carrying `apps -> ../../../../etc`. Path
// "apps/passage.yaml" passes the lexical check (it contains no "..") and only
// the filesystem resolution reveals the escape.
func TestGitPublishRefusesASymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	origin := originAuthorRepoWithSymlink(t, "apps", escapeTarget(outside))

	_, err := gitPublish(context.Background(), publishRequest{
		CloneURL: origin,
		Head:     "hecate/author-test",
		Path:     "apps/passage.yaml",
		Content:  []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n"),
	})
	if err == nil {
		t.Fatal("expected a refusal — apps is a symlink out of the checkout")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "passage.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("the write escaped the checkout: %s exists", filepath.Join(outside, "passage.yaml"))
	}
}

// TestGitPublishRefusesAnExistingSymlink is the other half: a symlink
// planted *at* Path itself, rather than in a parent directory — refusing it
// outright (not folded into the exists/Overwrite decision) is what stops a
// write from going through it to wherever it points.
func TestGitPublishRefusesAnExistingSymlink(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.yaml"), []byte("kind: Gate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := escapeTarget(filepath.Join(outside, "target.yaml"))
	origin := originAuthorRepoWithSymlink(t, "passage.yaml", target)

	_, err := gitPublish(context.Background(), publishRequest{
		CloneURL:  origin,
		Head:      "hecate/author-test",
		Path:      "passage.yaml",
		Content:   []byte("apiVersion: hecate.dev/v1alpha1\nkind: Passage\n"),
		Overwrite: true, // even asking to overwrite must not follow the symlink
	})
	if err == nil {
		t.Fatal("expected a refusal — passage.yaml is itself a symlink")
	}
	got, readErr := os.ReadFile(filepath.Join(outside, "target.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "kind: Gate\n" {
		t.Fatalf("the symlink's target was overwritten: %s", got)
	}
}

// originAuthorRepo creates a bare repository seeded with the given files, the
// same file://-free technique pkg/passage/steps/git_test.go uses so nothing
// here touches a real remote.
func originAuthorRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")

	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(seed, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@example.com"}
	if _, err := tree.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainClone(origin, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return origin
}

// originAuthorRepoWithSymlink is originAuthorRepo, but the seed also commits
// one symlink named linkName pointing at target. go-git tracks a symlink as
// an ordinary blob (git mode 120000) and recreates it as a real symlink on
// checkout, the same as any other git client would — this is what lets a
// pull-request author plant one on purpose.
func originAuthorRepoWithSymlink(t *testing.T, linkName, target string) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")

	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "demo-pipeline.yaml"), []byte("kind: Gate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(seed, linkName)); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@example.com"}
	if _, err := tree.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainClone(origin, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return origin
}

// TestAuthorPassageValidatesTheRequest is table-driven over the fields that
// have no reasonable default, in the idiom of the other write-endpoint tests
// in this package.
func TestAuthorPassageValidatesTheRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no name", `{"gate":"staging","bundle":"b1","steps":` + validSteps +
			`,"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"no gate", `{"name":"p1","bundle":"b1","steps":` + validSteps +
			`,"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"no bundle", `{"name":"p1","gate":"staging","steps":` + validSteps +
			`,"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"no steps", `{"name":"p1","gate":"staging","bundle":"b1","repo":"https://github.com/acme/fleet.git",` +
			`"path":"a.yaml","credentialsRef":"forge"}`},
		{"empty steps", `{"name":"p1","gate":"staging","bundle":"b1","steps":[],` +
			`"repo":"https://github.com/acme/fleet.git","path":"a.yaml","credentialsRef":"forge"}`},
		{"no repo", `{"name":"p1","gate":"staging","bundle":"b1","steps":` + validSteps +
			`,"path":"a.yaml","credentialsRef":"forge"}`},
		{"no path", `{"name":"p1","gate":"staging","bundle":"b1","steps":` + validSteps +
			`,"repo":"https://github.com/acme/fleet.git","credentialsRef":"forge"}`},
		{"no credentialsRef", `{"name":"p1","gate":"staging","bundle":"b1","steps":` + validSteps +
			`,"repo":"https://github.com/acme/fleet.git","path":"a.yaml"}`},
		{"unroutable repo", `{"name":"p1","gate":"staging","bundle":"b1","steps":` + validSteps +
			`,"repo":"not a url","path":"a.yaml","credentialsRef":"forge"}`},
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
