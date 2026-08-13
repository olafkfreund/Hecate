package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	hgit "github.com/olafkfreund/hecate/pkg/git"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// Step names.
const (
	StepGitClone  = "git-clone"
	StepGitCommit = "git-commit"
	StepGitPush   = "git-push"
)

// Failure reasons. Stable codes per D21, so a failure can be classified without
// reading English.
const (
	// ReasonGitAuthFailed means the host rejected our credentials. Distinct
	// from unreachable: one needs a new token, the other needs the network.
	ReasonGitAuthFailed = "GitAuthFailed"
	// ReasonGitUnreachable means the remote could not be contacted.
	ReasonGitUnreachable = "GitUnreachable"
	// ReasonGitRejected means the remote refused the push — usually because the
	// branch moved under us.
	ReasonGitRejected = "GitRejected"
	// ReasonWorkDirLost means an earlier step's checkout is gone. Scratch space
	// is deliberately disposable (D19), so this is what a controller restart
	// mid-crossing looks like from here.
	ReasonWorkDirLost = "WorkDirLost"
	// ReasonGitFailed is any other git failure.
	ReasonGitFailed = "GitFailed"
)

// defaultCheckout is where git-clone puts the working tree when no path is
// given, and where the other git steps look for it.
const defaultCheckout = "repo"

// gitAuth resolves credentials for a repository.
//
// A thin wrapper over pkg/git, which a Beacon watching the same repository
// uses too. Two answers to "what does this Secret mean?" is how a Beacon ends
// up unable to see a repository its own promotion step writes to daily.
type gitAuth struct{ client client.Client }

func (g gitAuth) resolve(
	ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef,
) (transport.AuthMethod, error) {
	return hgit.Auth(ctx, g.client, namespace, ref)
}

// classify maps a git error to a stable reason code.
func classify(err error) string {
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return ReasonGitAuthFailed
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return ReasonGitUnreachable
	case errors.Is(err, git.ErrNonFastForwardUpdate):
		return ReasonGitRejected
	}
	// go-git reports transport problems as opaque strings often enough that a
	// substring check is the honest fallback.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authentication"), strings.Contains(msg, "authorization"),
		strings.Contains(msg, "permission denied"):
		return ReasonGitAuthFailed
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "could not read"), strings.Contains(msg, "dial tcp"):
		return ReasonGitUnreachable
	case strings.Contains(msg, "non-fast-forward"), strings.Contains(msg, "rejected"):
		return ReasonGitRejected
	}
	return ReasonGitFailed
}

// checkoutPath resolves a step's `path` against the Passage work dir, refusing
// anything that escapes it.
func checkoutPath(workDir, path string) (string, error) {
	if path == "" {
		path = defaultCheckout
	}
	full := filepath.Join(workDir, path)
	// A `path` of "../../etc" would otherwise write outside the scratch
	// directory. Steps take configuration from a Gate, which is not necessarily
	// authored by whoever runs the controller.
	if rel, err := filepath.Rel(workDir, full); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes the work directory", path)
	}
	return full, nil
}

// openRepo opens an existing checkout, distinguishing "not there" from "broken".
func openRepo(dir string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, passage.FailTerminal(ReasonWorkDirLost,
			"no git checkout at %s — an earlier git-clone step's work is gone, "+
				"which happens when the controller restarts mid-crossing; retry the crossing",
			dir)
	}
	if err != nil {
		return nil, passage.FailTerminal(ReasonGitFailed, "opening %s: %s", dir, err)
	}
	return repo, nil
}

// ---------------------------------------------------------------- clone ----

// GitCloneConfig is the `with:` block of a git-clone step.
type GitCloneConfig struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	// Path is where the working tree goes, relative to the Passage work dir.
	Path string `json:"path,omitempty"`
	// Depth limits history. Zero clones everything; 1 is enough to commit on
	// top of the tip and is much faster on a large repository.
	Depth          int                      `json:"depth,omitempty"`
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// GitClone checks out a repository into the Passage work dir.
type GitClone struct{ auth gitAuth }

// NewGitClone returns a git-clone step.
func NewGitClone(c client.Client) *GitClone { return &GitClone{auth: gitAuth{client: c}} }

// Name implements passage.Runner.
func (g *GitClone) Name() string { return StepGitClone }

// Run implements passage.Runner.
//
// Re-entrant by design (D19): an existing checkout of the same repository is
// reused rather than re-cloned, and a missing one is cloned. A step that
// assumed the directory was fresh would work in testing and fail after a
// controller restart.
func (g *GitClone) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[GitCloneConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitClone, err)
	}
	if cfg.Repo == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: repo is required", StepGitClone)
	}
	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitClone, err)
	}

	auth, err := g.auth.resolve(ctx, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitClone, err)
	}

	repo, reused, err := g.obtain(ctx, dir, cfg, auth)
	if err != nil {
		return passage.StepResult{}, err
	}

	head, err := repo.Head()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: reading HEAD: %s", StepGitClone, err)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("checked out %s at %s", cfg.Repo, head.Hash().String()[:8]),
		Output: map[string]any{
			"path":   dir,
			"sha":    head.Hash().String(),
			"branch": head.Name().Short(),
			"reused": reused,
		},
	}, nil
}

func (g *GitClone) obtain(
	ctx context.Context, dir string, cfg GitCloneConfig, auth transport.AuthMethod,
) (*git.Repository, bool, error) {
	if repo, err := git.PlainOpen(dir); err == nil {
		// Reuse only if it is the same remote. A leftover checkout of a
		// different repository would silently commit to the wrong place.
		if remote, err := repo.Remote(git.DefaultRemoteName); err == nil {
			for _, u := range remote.Config().URLs {
				if u == cfg.Repo {
					return repo, true, nil
				}
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, false, passage.FailTerminal(ReasonGitFailed,
				"%s: removing a checkout of a different repository: %s", StepGitClone, err)
		}
	}

	opts := &git.CloneOptions{URL: cfg.Repo, Auth: auth, Depth: cfg.Depth}
	if cfg.Branch != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(cfg.Branch)
		opts.SingleBranch = true
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, opts)
	if err != nil {
		reason := classify(err)
		// Auth and a missing repository will not fix themselves; a network
		// blip might, so that one stays retryable.
		if reason == ReasonGitAuthFailed {
			return nil, false, passage.FailTerminal(reason, "%s: cloning %s: %s", StepGitClone, cfg.Repo, err)
		}
		return nil, false, passage.Fail(reason, "%s: cloning %s: %s", StepGitClone, cfg.Repo, err)
	}
	return repo, false, nil
}

// --------------------------------------------------------------- commit ----

// GitCommitConfig is the `with:` block of a git-commit step.
type GitCommitConfig struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	// Paths limits what is staged. Empty stages every change.
	Paths []string `json:"paths,omitempty"`
	// Author defaults to Hecate itself.
	Author *GitAuthor `json:"author,omitempty"`
	// AllowEmpty commits even with no changes. Off by default: an empty commit
	// usually means an earlier edit step did nothing, which is worth noticing
	// rather than burying under a commit that changes no files.
	AllowEmpty bool `json:"allowEmpty,omitempty"`
}

// GitAuthor identifies a commit's author.
type GitAuthor struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// GitCommit stages and commits the working tree.
type GitCommit struct{}

// NewGitCommit returns a git-commit step.
func NewGitCommit() *GitCommit { return &GitCommit{} }

// Name implements passage.Runner.
func (g *GitCommit) Name() string { return StepGitCommit }

// Run implements passage.Runner.
//
// The commit is **deterministic**: its timestamps come from the Passage's start
// rather than the wall clock, so the same tree on the same parent with the same
// message yields the same SHA every time. Re-running a crossing therefore
// re-creates the identical commit instead of stacking a second one on the
// branch, and a re-push becomes a no-op.
func (g *GitCommit) Run(_ context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[GitCommitConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitCommit, err)
	}
	if cfg.Message == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: message is required", StepGitCommit)
	}
	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitCommit, err)
	}

	repo, err := openRepo(dir)
	if err != nil {
		return passage.StepResult{}, err
	}
	tree, err := repo.Worktree()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: %s", StepGitCommit, err)
	}

	if err := stage(tree, cfg.Paths); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: staging: %s", StepGitCommit, err)
	}

	status, err := tree.Status()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: %s", StepGitCommit, err)
	}
	if status.IsClean() && !cfg.AllowEmpty {
		// Not a failure: the desired state is already in the repository, which
		// is exactly what re-running a crossing looks like. Report the existing
		// HEAD so later steps still have a revision to wait for.
		head, err := repo.Head()
		if err != nil {
			return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: %s", StepGitCommit, err)
		}
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: "nothing to commit; the tree already matches",
			Output: map[string]any{
				"sha": head.Hash().String(), "short": head.Hash().String()[:8], "committed": false,
			},
		}, nil
	}

	sig := signature(cfg.Author, sc.StartedAt)
	hash, err := tree.Commit(withTraceparent(cfg.Message, sc.Traceparent), &git.CommitOptions{
		Author: sig, Committer: sig, AllowEmptyCommits: cfg.AllowEmpty,
	})
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: %s", StepGitCommit, err)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("committed %s", hash.String()[:8]),
		Output: map[string]any{
			"sha": hash.String(), "short": hash.String()[:8], "committed": true,
		},
	}, nil
}

// withTraceparent appends the W3C trace context as a git trailer.
//
// Git is the rendezvous (D3), so git is where the trace context has to travel:
// this is the link that lets one trace span the CI run that built the artifact,
// the crossing that promoted it, and the reconciliation that applied it.
//
// A trailer is the right shape for it — a `key: value` line in the message's
// last paragraph, which `git interpret-trailers` and every forge already
// understand — and it changes nothing for a reader who does not care.
//
// The trace ID is allocated once per Passage and persisted, so a retried
// crossing writes the identical trailer and therefore the identical commit SHA.
// Generating it here instead would give every attempt a new hash and turn a
// harmless retry into a second commit on the branch.
func withTraceparent(message, traceparent string) string {
	if traceparent == "" {
		return message
	}
	// A trailer must be its own paragraph, or git reads it as prose.
	return strings.TrimRight(message, "\n") + "\n\ntraceparent: " + traceparent + "\n"
}

// stage adds the configured paths, or everything when none are given.
func stage(tree *git.Worktree, paths []string) error {
	if len(paths) == 0 {
		// AddWithOptions(All) also records deletions, which AddGlob does not —
		// a promotion that removes a manifest must not silently keep it.
		return tree.AddWithOptions(&git.AddOptions{All: true})
	}
	for _, p := range paths {
		if err := tree.AddWithOptions(&git.AddOptions{Path: p, All: true}); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// signature builds the commit identity, with timestamps fixed to the Passage
// start so the commit hash is reproducible.
func signature(author *GitAuthor, startedAt time.Time) *object.Signature {
	name, email := "Hecate", "hecate@users.noreply.github.com"
	if author != nil {
		if author.Name != "" {
			name = author.Name
		}
		if author.Email != "" {
			email = author.Email
		}
	}
	when := startedAt
	if when.IsZero() {
		// Only when a caller did not supply one — determinism is lost, but a
		// zero timestamp would date every commit to 1970.
		when = time.Now()
	}
	return &object.Signature{Name: name, Email: email, When: when.UTC()}
}

// ----------------------------------------------------------------- push ----

// GitPushConfig is the `with:` block of a git-push step.
type GitPushConfig struct {
	Path string `json:"path,omitempty"`
	// Branch overrides the target branch. Empty pushes the checked-out branch.
	Branch string `json:"branch,omitempty"`
	// ToNewBranch pushes to a fresh branch named after the Passage, for flows
	// that open a pull request rather than committing to the trunk.
	ToNewBranch    bool                     `json:"toNewBranch,omitempty"`
	Force          bool                     `json:"force,omitempty"`
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// GitPush publishes the local commits.
type GitPush struct{ auth gitAuth }

// NewGitPush returns a git-push step.
func NewGitPush(c client.Client) *GitPush { return &GitPush{auth: gitAuth{client: c}} }

// Name implements passage.Runner.
func (g *GitPush) Name() string { return StepGitPush }

// Run implements passage.Runner.
func (g *GitPush) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[GitPushConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPush, err)
	}
	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPush, err)
	}

	repo, err := openRepo(dir)
	if err != nil {
		return passage.StepResult{}, err
	}
	head, err := repo.Head()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonGitFailed, "%s: reading HEAD: %s", StepGitPush, err)
	}

	target := cfg.Branch
	if cfg.ToNewBranch {
		// Named after the Passage, which is unique per attempt and traceable
		// back to the crossing that opened it.
		target = "hecate/" + sc.Passage
	}
	if target == "" {
		target = head.Name().Short()
	}

	auth, err := g.auth.resolve(ctx, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPush, err)
	}

	spec := config.RefSpec(fmt.Sprintf("%s:%s", head.Name(), plumbing.NewBranchReferenceName(target)))
	if cfg.Force {
		spec = config.RefSpec("+" + string(spec))
	}
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName, RefSpecs: []config.RefSpec{spec}, Auth: auth,
	})

	switch {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		// The expected outcome of re-running a crossing, because the commit is
		// deterministic: identical content produces the identical SHA, so there
		// is nothing to send. Success, not a failure.
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("%s already up to date at %s", target, head.Hash().String()[:8]),
			Output:  map[string]any{"branch": target, "sha": head.Hash().String(), "pushed": false},
		}, nil

	case err != nil:
		reason := classify(err)
		if reason == ReasonGitAuthFailed {
			return passage.StepResult{}, passage.FailTerminal(reason, "%s: %s", StepGitPush, err)
		}
		// A rejected push often means the branch moved; retrying after a fresh
		// clone can succeed, so it stays retryable.
		return passage.StepResult{}, passage.Fail(reason, "%s: pushing to %s: %s", StepGitPush, target, err)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("pushed %s to %s", head.Hash().String()[:8], target),
		Output:  map[string]any{"branch": target, "sha": head.Hash().String(), "pushed": true},
	}, nil
}

