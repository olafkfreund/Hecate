package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// passageStart is fixed so commits are reproducible across runs, which is the
// property these tests exist to protect.
var passageStart = time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

// originRepo creates a bare repository with one commit and returns its path.
// A real repository over file:// — go-git behaves the same as against a remote,
// and the test needs no network.
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")

	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "app.yaml"), []byte("image: acme:1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@example.com", When: passageStart.Add(-time.Hour)}
	if _, err := tree.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainClone(origin, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return origin
}

func gitCtx(t *testing.T, workDir string, cfg any) *passage.StepContext {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &passage.StepContext{
		Namespace: "acme", Gate: "production", Passage: "production-abc123",
		WorkDir: workDir, Config: raw, StartedAt: passageStart,
	}
}

func mustRun(t *testing.T, r passage.Runner, sc *passage.StepContext) passage.StepResult {
	t.Helper()
	res, err := r.Run(context.Background(), sc)
	if err != nil {
		t.Fatalf("%s: %v", r.Name(), err)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("%s: phase = %s (%s)", r.Name(), res.Phase, res.Message)
	}
	return res
}

func TestGitCloneChecksOut(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()

	res := mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))

	if _, err := os.Stat(filepath.Join(work, "repo", "app.yaml")); err != nil {
		t.Errorf("working tree is missing: %v", err)
	}
	if res.Output["sha"] == "" {
		t.Error("clone did not report the checked-out revision")
	}
	if res.Output["reused"] != false {
		t.Error("a fresh clone should not report reuse")
	}
}

// D19: a step must tolerate the work dir being in any state, including one an
// earlier invocation already populated.
func TestGitCloneIsReEntrant(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	sc := gitCtx(t, work, GitCloneConfig{Repo: origin})

	first := mustRun(t, NewGitClone(nil), sc)
	// Something an edit step would have written; reuse must not discard it.
	scratch := filepath.Join(work, "repo", "uncommitted.txt")
	if err := os.WriteFile(scratch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := mustRun(t, NewGitClone(nil), sc)

	if second.Output["reused"] != true {
		t.Error("a second clone of the same repository should reuse the checkout")
	}
	if first.Output["sha"] != second.Output["sha"] {
		t.Error("reuse changed the checked-out revision")
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Error("reuse destroyed work already in the checkout")
	}
}

// A leftover checkout of a different repository would silently commit to the
// wrong place.
func TestGitCloneReplacesADifferentRepository(t *testing.T) {
	first, second, work := originRepo(t), originRepo(t), t.TempDir()

	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: first}))
	res := mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: second}))

	if res.Output["reused"] != false {
		t.Error("a checkout of a different repository must be replaced, not reused")
	}
	repo, err := git.PlainOpen(filepath.Join(work, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := repo.Remote(git.DefaultRemoteName)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Config().URLs[0] != second {
		t.Errorf("remote = %s, want the second repository", remote.Config().URLs[0])
	}
}

// The property the whole model leans on: the same content produces the same
// commit, so re-running a crossing does not stack a second commit.
func TestCommitsAreDeterministic(t *testing.T) {
	shas := make([]string, 2)
	for i := range shas {
		origin, work := originRepo(t), t.TempDir()
		sc := gitCtx(t, work, GitCloneConfig{Repo: origin})
		mustRun(t, NewGitClone(nil), sc)

		if err := os.WriteFile(filepath.Join(work, "repo", "app.yaml"),
			[]byte("image: acme:2.0.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "promote to 2.0.0"}))
		shas[i] = res.Output["sha"].(string)
	}

	if shas[0] != shas[1] {
		t.Errorf("the same change produced different commits:\n  %s\n  %s\n"+
			"re-running a crossing would stack a second commit on the branch", shas[0], shas[1])
	}
}

func TestGitCommit(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))

	t.Run("commits a change", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(work, "repo", "app.yaml"),
			[]byte("image: acme:2.0.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "promote"}))

		if res.Output["committed"] != true {
			t.Error("a real change should report committed=true")
		}
		// The SHA is what flux-wait waits for, so it must be a full hash.
		sha, _ := res.Output["sha"].(string)
		if len(sha) != 40 {
			t.Errorf("sha = %q, want a full 40-character hash", sha)
		}
	})

	t.Run("a clean tree succeeds and reports HEAD", func(t *testing.T) {
		// Re-running a crossing looks exactly like this: the desired state is
		// already committed. Failing here would make retries impossible.
		res := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "promote"}))

		if res.Output["committed"] != false {
			t.Error("a clean tree should report committed=false")
		}
		if res.Output["sha"] == "" {
			t.Error("a clean tree must still report a revision for later steps to wait on")
		}
	})

	t.Run("deletions are staged", func(t *testing.T) {
		if err := os.Remove(filepath.Join(work, "repo", "app.yaml")); err != nil {
			t.Fatal(err)
		}
		mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "remove"}))

		repo, _ := git.PlainOpen(filepath.Join(work, "repo"))
		head, _ := repo.Head()
		commit, _ := repo.CommitObject(head.Hash())
		tree, _ := commit.Tree()
		if _, err := tree.File("app.yaml"); err == nil {
			t.Error("a removed file is still in the commit — deletions were not staged")
		}
	})
}

func TestGitPush(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))
	if err := os.WriteFile(filepath.Join(work, "repo", "app.yaml"),
		[]byte("image: acme:3.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "promote"}))

	res := mustRun(t, NewGitPush(nil), gitCtx(t, work, GitPushConfig{}))
	if res.Output["pushed"] != true {
		t.Error("the first push should report pushed=true")
	}

	// The commit really reached the origin.
	remote, err := git.PlainOpen(origin)
	if err != nil {
		t.Fatal(err)
	}
	head, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash().String() != commit.Output["sha"] {
		t.Errorf("origin is at %s, want the pushed commit %s", head.Hash(), commit.Output["sha"])
	}

	t.Run("pushing again is a no-op, not a failure", func(t *testing.T) {
		// The consequence of deterministic commits: a re-run has nothing to
		// send. Treating that as an error would make every retry fail.
		again := mustRun(t, NewGitPush(nil), gitCtx(t, work, GitPushConfig{}))
		if again.Output["pushed"] != false {
			t.Error("an up-to-date push should report pushed=false")
		}
	})
}

// The pull-request flow: push to a branch named after the Passage rather than
// the trunk.
func TestGitPushToNewBranch(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))
	if err := os.WriteFile(filepath.Join(work, "repo", "app.yaml"),
		[]byte("image: acme:4.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "promote"}))

	res := mustRun(t, NewGitPush(nil), gitCtx(t, work, GitPushConfig{ToNewBranch: true}))

	branch, _ := res.Output["branch"].(string)
	if !strings.Contains(branch, "production-abc123") {
		t.Errorf("branch = %q, should be traceable to the Passage", branch)
	}

	remote, _ := git.PlainOpen(origin)
	if _, err := remote.Reference(plumbing.NewBranchReferenceName(branch), true); err != nil {
		t.Errorf("branch %s does not exist on the origin: %v", branch, err)
	}
}

// A controller restart mid-crossing loses the scratch directory (D19). The
// failure must name what happened, not just fail to open a directory.
func TestMissingCheckoutIsExplained(t *testing.T) {
	work := t.TempDir()

	// Each step gets its own config: passing one step's config to another is a
	// mistake strict decoding now refuses, and it refused this test.
	for _, tc := range []struct {
		runner passage.Runner
		cfg    any
	}{
		{NewGitCommit(), GitCommitConfig{Message: "x"}},
		{NewGitPush(nil), GitPushConfig{}},
	} {
		r := tc.runner
		t.Run(r.Name(), func(t *testing.T) {
			_, err := r.Run(context.Background(), gitCtx(t, work, tc.cfg))
			if err == nil {
				t.Fatal("expected an error with no checkout present")
			}
			if got := passage.ReasonOf(err); got != ReasonWorkDirLost {
				t.Errorf("reason = %q, want %q", got, ReasonWorkDirLost)
			}
			if !strings.Contains(err.Error(), "retry") {
				t.Errorf("the message should say what to do: %v", err)
			}
		})
	}
}

// Step configuration comes from a Gate, which is not necessarily authored by
// whoever runs the controller.
func TestPathCannotEscapeTheWorkDir(t *testing.T) {
	work := t.TempDir()
	_, err := NewGitClone(nil).Run(context.Background(),
		gitCtx(t, work, GitCloneConfig{Repo: "https://example.com/x", Path: "../../etc"}))

	if err == nil {
		t.Fatal("a path escaping the work directory was accepted")
	}
	if got := passage.ReasonOf(err); got != ReasonInvalidConfig {
		t.Errorf("reason = %q, want %q", got, ReasonInvalidConfig)
	}
}

func TestGitStepsRejectBadConfig(t *testing.T) {
	work := t.TempDir()
	for name, tc := range map[string]struct {
		runner passage.Runner
		cfg    any
	}{
		"clone without a repo":     {NewGitClone(nil), GitCloneConfig{}},
		"commit without a message": {NewGitCommit(), GitCommitConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.runner.Run(context.Background(), gitCtx(t, work, tc.cfg))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !passage.IsTerminal(err) {
				t.Error("bad configuration must be terminal, not retried")
			}
			if got := passage.ReasonOf(err); got != ReasonInvalidConfig {
				t.Errorf("reason = %q, want %q", got, ReasonInvalidConfig)
			}
		})
	}
}

func TestClassifyGitErrors(t *testing.T) {
	// Auth needs a new token; unreachable needs the network. Conflating them
	// sends an operator to the wrong place.
	for msg, want := range map[string]string{
		"authentication required":                  ReasonGitAuthFailed,
		"ssh: handshake failed: permission denied": ReasonGitAuthFailed,
		"dial tcp 10.0.0.1:22: connection refused": ReasonGitUnreachable,
		"non-fast-forward update: refs/heads/main": ReasonGitRejected,
		"something else entirely":                  ReasonGitFailed,
	} {
		if got := classify(errors.New(msg)); got != want {
			t.Errorf("classify(%q) = %q, want %q", msg, got, want)
		}
	}
}
