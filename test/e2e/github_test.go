//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/olafkfreund/hecate/pkg/provider"
)

// The throwaway fleet repository this test writes to. Nothing in it is
// deployed; it exists so a promotion has somewhere real to push.
const (
	ghOwner = "olafkfreund"
	ghRepo  = "hecate-e2e-fleet"
	ghFile  = "apps/staging/configmap.yaml"
)

// TestGitHubPullRequestLifecycle is the half of a promotion that plain git
// cannot do.
//
// Clone, commit and push are already proven against a real Gitea on every run,
// and they need no provider code at all — git over HTTPS is the same everywhere.
// What is untested until here is the API surface: opening a pull request,
// finding the one already open rather than opening a second, reporting commit
// status, and reading a merge back with the commit it produced.
//
// Against real GitHub, deliberately. A recorded fixture proves we still agree
// with a copy of GitHub from the day it was recorded, which is exactly the
// failure mode this is meant to catch.
//
// Skipped without HECATE_E2E_GITHUB_TOKEN, so the suite stays runnable by
// anyone without credentials to a third party.
func TestGitHubPullRequestLifecycle(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("HECATE_E2E_GITHUB_TOKEN"))
	if token == "" {
		t.Skip("no HECATE_E2E_GITHUB_TOKEN; skipping the GitHub provider e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p, err := provider.New(provider.GitHub, provider.Config{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	repo := provider.Repo{Host: "github.com", Owner: ghOwner, Name: ghRepo}

	// Unique per run, so two runs never collide on a branch and a failed run
	// leaves evidence rather than being overwritten by the next one.
	branch := fmt.Sprintf("e2e/%d", time.Now().UnixNano())
	sha := pushABump(t, token, repo, branch)
	t.Cleanup(func() { deleteBranch(t, token, repo, branch) })

	pr, err := p.EnsurePullRequest(ctx, provider.PullRequestSpec{
		Repo: repo, Head: branch, Base: "main",
		Title: "e2e: promote to staging",
		Body:  "Opened by Hecate's GitHub end-to-end test. Safe to close.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number == 0 || pr.State != provider.Open {
		t.Fatalf("pull request = %+v, want an open one with a number", pr)
	}

	// D19: a step is re-entrant, so this runs again after every requeue. A
	// second identical pull request is worse than none — reviewers get two,
	// and merging one leaves the other open against a stale branch.
	//
	// What this can and cannot prove: GitHub refuses a second pull request for
	// the same head branch itself, so deleting the adapter's own lookup still
	// passes here — the 422 fallback catches it. The two paths are outwardly
	// identical and no assertion separates them. What this does catch is the
	// lookup returning the *wrong* pull request, which is the failure that
	// actually bit: an unqualified head matches every repository's branch of
	// that name.
	again, err := p.EnsurePullRequest(ctx, provider.PullRequestSpec{
		Repo: repo, Head: branch, Base: "main", Title: "e2e: promote to staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Number != pr.Number {
		t.Errorf("asking twice opened #%d and #%d — a requeue would open one per reconcile",
			pr.Number, again.Number)
	}

	// Twice, for the same reason: reporting the same state again must succeed
	// rather than fail the crossing over a duplicate.
	for i := range 2 {
		err := p.SetCommitStatus(ctx, provider.CommitStatus{
			Repo: repo, SHA: sha, State: provider.StatePending,
			Context: "hecate/staging", Description: "crossing staging",
		})
		if err != nil {
			t.Fatalf("setting commit status (attempt %d): %v", i+1, err)
		}
	}

	mergeSHA := merge(t, token, repo, pr.Number)

	got, err := p.PullRequest(ctx, repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.Merged {
		t.Errorf("state = %q after merging, want Merged — a crossing would wait for ever", got.State)
	}
	// The branch commit never lands on main under its own hash when the host
	// squashes, so a flux-wait keyed on `sha` would wait for a commit that
	// will never appear. This is the one it must wait for.
	if got.MergeCommit == "" {
		t.Error("no merge commit — flux-wait has nothing to wait for")
	}
	if got.MergeCommit != mergeSHA {
		t.Errorf("merge commit = %q, GitHub merged %q", got.MergeCommit, mergeSHA)
	}
}

// pushABump clones the fleet, rewrites the tag and pushes a branch, using the
// same go-git transport the git steps use rather than a shell git — so this
// exercises the product's own path to GitHub, not a second one.
func pushABump(t *testing.T, token string, repo provider.Repo, branch string) string {
	t.Helper()
	auth := &githttp.BasicAuth{Username: "x-access-token", Password: token}
	url := fmt.Sprintf("https://github.com/%s.git", repo.Slug())

	dir := t.TempDir()
	r, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url, Auth: auth, Depth: 1})
	if err != nil {
		t.Fatalf("cloning %s: %v", url, err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch), Create: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-github-payload
data:
  tag: "1.%d.0"
`, time.Now().Unix()%1000)
	if err := os.WriteFile(dir+"/"+ghFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(ghFile); err != nil {
		t.Fatal(err)
	}
	commit, err := w.Commit("e2e: bump staging", &git.CommitOptions{
		Author: &object.Signature{Name: "hecate-e2e", Email: "e2e@hecate.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Push(&git.PushOptions{
		Auth: auth,
		RefSpecs: []config.RefSpec{config.RefSpec(fmt.Sprintf(
			"refs/heads/%s:refs/heads/%s", branch, branch))},
	}); err != nil {
		t.Fatalf("pushing %s: %v", branch, err)
	}
	return commit.String()
}

// merge merges the pull request and returns the commit GitHub produced.
//
// Plain net/http rather than the provider: Hecate does not merge. A crossing
// opens a pull request and *waits* for a human or a rule to merge it, which is
// the point of gating on review at all — so the test plays the human.
func merge(t *testing.T, token string, repo provider.Repo, number int) string {
	t.Helper()
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	ghAPI(t, token, http.MethodPut,
		fmt.Sprintf("/repos/%s/pulls/%d/merge", repo.Slug(), number),
		map[string]any{"merge_method": "squash"}, &out)
	if !out.Merged {
		t.Fatalf("GitHub did not merge #%d", number)
	}
	return out.SHA
}

// deleteBranch tidies up. Best-effort: a leftover branch in a throwaway repo is
// untidy, whereas failing the test over it would report a passing lifecycle as
// broken.
func deleteBranch(t *testing.T, token string, repo provider.Repo, branch string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("https://api.github.com/repos/%s/git/refs/heads/%s", repo.Slug(), branch), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("could not delete %s: %v", branch, err)
		return
	}
	_ = resp.Body.Close()
}

func ghAPI(t *testing.T, token, method, path string, body, out any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
	}
}
