package beacon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// testGitRepo is a real repository on disk with a real history, which is the
// only way to test path filtering: the question "what did this commit change?"
// is answered by git's own tree diff, and a fake would be answering it for us.
type testGitRepo struct {
	t    *testing.T
	Dir  string
	repo *gogit.Repository
	n    int
}

func newTestGitRepo(t *testing.T) *testGitRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &testGitRepo{t: t, Dir: dir, repo: repo}
}

// Commit writes each file with unique content and commits them.
func (g *testGitRepo) Commit(message string, files ...string) plumbing.Hash {
	g.t.Helper()
	w, err := g.repo.Worktree()
	if err != nil {
		g.t.Fatal(err)
	}
	g.n++
	for _, f := range files {
		if err := writeUnder(g.Dir, f, g.n); err != nil {
			g.t.Fatal(err)
		}
		if _, err := w.Add(f); err != nil {
			g.t.Fatal(err)
		}
	}
	// Distinct timestamps, so "newest" is not decided by which commit the test
	// happened to write first.
	h, err := w.Commit(message, &gogit.CommitOptions{Author: &object.Signature{
		Name: "test", Email: "test@hecate.test", When: time.Now().Add(time.Duration(g.n) * time.Second),
	}})
	if err != nil {
		g.t.Fatal(err)
	}
	return h
}

func (g *testGitRepo) Tag(name string, h plumbing.Hash) {
	g.t.Helper()
	if _, err := g.repo.CreateTag(name, h, nil); err != nil {
		g.t.Fatal(err)
	}
}

// AnnotatedTag makes a tag object rather than a lightweight ref, so the peeled
// commit differs from the tag's own hash.
func (g *testGitRepo) AnnotatedTag(name string, h plumbing.Hash) {
	g.t.Helper()
	_, err := g.repo.CreateTag(name, h, &gogit.CreateTagOptions{
		Message: name,
		Tagger:  &object.Signature{Name: "test", Email: "test@hecate.test", When: time.Now()},
	})
	if err != nil {
		g.t.Fatal(err)
	}
}

// Branch names the current head, since PlainInit's default may be master or
// main depending on the go-git version.
func (g *testGitRepo) Branch(name string) {
	g.t.Helper()
	head, err := g.repo.Head()
	if err != nil {
		g.t.Fatal(err)
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), head.Hash())
	if err := g.repo.Storer.SetReference(ref); err != nil {
		g.t.Fatal(err)
	}
}

func writeUnder(dir, file string, n int) error {
	full := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// Unique content per commit, so a rewrite of the same path is a real change
	// rather than a no-op git would refuse to commit.
	return os.WriteFile(full, []byte(strings.Repeat("x", n)+"\n"), 0o644)
}

func resolveGitWatch(t *testing.T, w v1alpha1.GitWatch) (*v1alpha1.CommitArtifact, error) {
	t.Helper()
	got, err := (&Resolver{}).Resolve(context.Background(), "acme", v1alpha1.WatchSource{Git: &w})
	if err != nil {
		return nil, err
	}
	if got.Commit == nil {
		t.Fatalf("resolved to %+v, which is not a commit", got)
	}
	return got.Commit, nil
}

func TestGitResolvesABranchHead(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("first", "apps/checkout/deploy.yaml")
	head := g.Commit("second", "apps/search/deploy.yaml")
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{Repo: g.Dir, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// The commit, not the branch name: a branch moves, and a promotion has to
	// pin what was actually deployed.
	if got.SHA != head.String() {
		t.Errorf("sha = %s, want the branch head %s", got.SHA, head)
	}
	if got.Branch != "main" {
		t.Errorf("branch = %q", got.Branch)
	}
}

func TestGitReportsAMissingBranch(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("first", "a.yaml")
	g.Branch("main")

	_, err := resolveGitWatch(t, v1alpha1.GitWatch{Repo: g.Dir, Branch: "release"})
	if err == nil || !strings.Contains(err.Error(), "no branch") {
		t.Errorf("error = %v, want the missing branch named", err)
	}
}

func TestGitResolvesTheNewestTag(t *testing.T) {
	g := newTestGitRepo(t)
	old := g.Commit("first", "a.yaml")
	newer := g.Commit("second", "b.yaml")
	g.Tag("v1.9.0", old)
	g.Tag("v1.10.0", newer)

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{Repo: g.Dir, Tags: &v1alpha1.TagWatch{}})
	if err != nil {
		t.Fatal(err)
	}
	// 1.10.0 over 1.9.0: SemVer is the default, and lexical would downgrade.
	if got.Tag != "v1.10.0" || got.SHA != newer.String() {
		t.Errorf("tag = %q sha = %s, want v1.10.0 at %s", got.Tag, got.SHA, newer)
	}
}

// An annotated tag points at a tag object. Pinning that hash would give a SHA
// no checkout ever produces, and every downstream comparison would miss.
func TestGitPeelsAnAnnotatedTag(t *testing.T) {
	g := newTestGitRepo(t)
	commit := g.Commit("first", "a.yaml")
	g.AnnotatedTag("v2.0.0", commit)

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{Repo: g.Dir, Tags: &v1alpha1.TagWatch{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != commit.String() {
		t.Errorf("sha = %s, want the commit %s rather than the tag object", got.SHA, commit)
	}
}

func TestGitHonoursTheTagConstraint(t *testing.T) {
	g := newTestGitRepo(t)
	six := g.Commit("first", "a.yaml")
	seven := g.Commit("second", "b.yaml")
	g.Tag("v6.14.1", six)
	g.Tag("v7.0.0", seven)

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Tags: &v1alpha1.TagWatch{Constraint: "^6.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v6.14.1" {
		t.Errorf("tag = %q — the constraint exists to stop a major arriving unasked", got.Tag)
	}
}

// The heart of #106: a monorepo must not promote every service on every commit.
func TestGitPathsIgnoreACommitElsewhere(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("checkout: first", "apps/checkout/deploy.yaml")
	wanted := g.Commit("checkout: second", "apps/checkout/deploy.yaml")
	g.Commit("search: unrelated", "apps/search/deploy.yaml")
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Paths: []string{"apps/checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The head changed search, so the checkout watch must still resolve to the
	// last commit that changed checkout — the same SHA as before, which is what
	// makes it emit no new Bundle.
	if got.SHA != wanted.String() {
		t.Errorf("sha = %s, want %s — a commit to another service moved this watch", got.SHA, wanted)
	}
	if got.Message != "checkout: second" {
		t.Errorf("message = %q", got.Message)
	}
}

// Walking back rather than refusing is what lets a watch describe what is there
// now, not only what has changed since it was created.
func TestGitPathsResolveOnARepoWhoseHeadIsUnrelated(t *testing.T) {
	g := newTestGitRepo(t)
	wanted := g.Commit("checkout", "apps/checkout/deploy.yaml")
	for range 5 {
		g.Commit("noise", "docs/readme.md")
	}
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Paths: []string{"apps/checkout"},
	})
	if err != nil {
		t.Fatalf("a new Beacon on a quiet path resolved to nothing: %v", err)
	}
	if got.SHA != wanted.String() {
		t.Errorf("sha = %s, want %s", got.SHA, wanted)
	}
}

func TestGitPathsMatchTheDirectoryItself(t *testing.T) {
	g := newTestGitRepo(t)
	// A file at exactly the watched path, not under it.
	wanted := g.Commit("chart", "apps/checkout")
	g.Commit("noise", "docs/readme.md")
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Paths: []string{"apps/checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != wanted.String() {
		t.Errorf("sha = %s, want %s", got.SHA, wanted)
	}
}

// A prefix that is not a path boundary must not match: apps/checkout-api is a
// different service from apps/checkout.
func TestGitPathsDoNotMatchASiblingWithTheSamePrefix(t *testing.T) {
	g := newTestGitRepo(t)
	wanted := g.Commit("checkout", "apps/checkout/deploy.yaml")
	g.Commit("a different service", "apps/checkout-api/deploy.yaml")
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Paths: []string{"apps/checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != wanted.String() {
		t.Errorf("sha = %s — apps/checkout-api was treated as part of apps/checkout", got.SHA)
	}
}

func TestGitIgnorePathsCarveOutOfAWatch(t *testing.T) {
	g := newTestGitRepo(t)
	wanted := g.Commit("checkout", "apps/checkout/deploy.yaml")
	g.Commit("docs only", "apps/checkout/docs/README.md")
	g.Branch("main")

	got, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo:        g.Dir,
		Branch:      "main",
		Paths:       []string{"apps/checkout"},
		IgnorePaths: []string{"apps/checkout/docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != wanted.String() {
		t.Errorf("sha = %s — a docs-only commit moved the watch", got.SHA)
	}
}

func TestGitSaysWhenNothingInTheWindowTouchedThePaths(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("noise", "docs/readme.md")
	g.Branch("main")

	_, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Paths: []string{"apps/checkout"},
	})
	var noMatch *ErrNoMatch
	if !errors.As(err, &noMatch) {
		t.Fatalf("error = %T %v, want ErrNoMatch", err, err)
	}
	// A watch on a path nothing has touched is correctly configured and simply
	// has nothing to offer, which is not the same as a broken repository.
	if !strings.Contains(err.Error(), "apps/checkout") {
		t.Errorf("error = %v, want the paths named", err)
	}
}

func TestGitRefusesBranchAndTagsTogether(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("first", "a.yaml")
	g.Branch("main")

	_, err := resolveGitWatch(t, v1alpha1.GitWatch{
		Repo: g.Dir, Branch: "main", Tags: &v1alpha1.TagWatch{},
	})
	// Two answers to "what is newest". Picking one silently would ignore
	// something the author wrote down.
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v", err)
	}
}

func TestGitRefusesNeitherBranchNorTags(t *testing.T) {
	g := newTestGitRepo(t)
	g.Commit("first", "a.yaml")

	_, err := resolveGitWatch(t, v1alpha1.GitWatch{Repo: g.Dir})
	if err == nil || !strings.Contains(err.Error(), "either branch or tags") {
		t.Errorf("error = %v", err)
	}
}
