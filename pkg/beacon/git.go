package beacon

import (
	"context"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	hgit "github.com/olafkfreund/hecate/pkg/git"
)

// historyLimit bounds how far back a path filter will look.
//
// ponytail: a shallow clone of this depth, walked linearly. A path nobody has
// touched in 500 commits resolves to nothing and says so, rather than the
// Beacon quietly fetching an entire monorepo's history every poll. The upgrade
// path, if a real repository needs it, is a deeper clone — not an unbounded
// one.
const historyLimit = 200

// resolveGit finds the commit a GitWatch currently points at.
//
// Two shapes: follow a branch's head, or take the newest tag. Both answer with
// a commit, because a commit is what a promotion pins — a branch name moves and
// a tag can be moved, and neither is evidence of what was deployed.
func (r *Resolver) resolveGit(
	ctx context.Context, namespace string, w v1alpha1.GitWatch,
) (v1alpha1.Artifact, error) {
	switch {
	case w.Branch == "" && w.Tags == nil:
		return v1alpha1.Artifact{}, fmt.Errorf(
			"git watch on %s: set either branch or tags", w.Repo)
	case w.Branch != "" && w.Tags != nil:
		// Both would mean two different answers to "what is newest", and
		// picking one silently would ignore something the author wrote down.
		return v1alpha1.Artifact{}, fmt.Errorf(
			"git watch on %s: branch and tags are mutually exclusive", w.Repo)
	}

	auth, err := hgit.Auth(ctx, r.Client, namespace, w.CredentialsRef)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}

	refs, err := listRefs(ctx, w.Repo, auth)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}

	artifact := v1alpha1.CommitArtifact{Repo: w.Repo}
	var head plumbing.Hash
	if w.Branch != "" {
		artifact.Branch = w.Branch
		head, err = branchHead(refs, w.Repo, w.Branch)
	} else {
		artifact.Tag, head, err = newestTag(refs, w.Repo, *w.Tags)
	}
	if err != nil {
		return v1alpha1.Artifact{}, err
	}
	artifact.SHA = head.String()

	// No path filter: the ref itself is the answer, and listing refs has
	// already told us everything. Fetching a repository to learn what we
	// already know would make every Beacon poll clone a monorepo.
	if len(w.Paths) == 0 && len(w.IgnorePaths) == 0 {
		return v1alpha1.Artifact{Commit: &artifact}, nil
	}

	sha, subject, err := r.newestTouching(ctx, w, auth, head)
	if err != nil {
		return v1alpha1.Artifact{}, err
	}
	artifact.SHA = sha
	artifact.Message = subject
	return v1alpha1.Artifact{Commit: &artifact}, nil
}

// listRefs asks the remote what it has, without cloning it.
func listRefs(ctx context.Context, repo string, auth transport.AuthMethod) ([]*plumbing.Reference, error) {
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin", URLs: []string{repo},
	})
	// AppendPeeled, because the default is IgnorePeeled and an annotated tag
	// would then resolve to the tag object rather than the commit — a SHA no
	// checkout of the working tree ever produces, so every downstream
	// comparison would miss.
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{
		Auth: auth, PeelingOption: gogit.AppendPeeled,
	})
	if err != nil {
		return nil, fmt.Errorf("listing refs of %s: %w", repo, err)
	}
	return refs, nil
}

func branchHead(refs []*plumbing.Reference, repo, branch string) (plumbing.Hash, error) {
	want := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == want {
			return ref.Hash(), nil
		}
	}
	return plumbing.ZeroHash, fmt.Errorf("%s has no branch %q", repo, branch)
}

// newestTag picks a tag with the shared Selection logic and returns the commit
// it points at.
func newestTag(
	refs []*plumbing.Reference, repo string, w v1alpha1.TagWatch,
) (string, plumbing.Hash, error) {
	// An annotated tag points at a tag object, not a commit, and the peeled
	// ref (refs/tags/x^{}) is the commit. Both appear in a ref listing, so the
	// peeled one is preferred where it exists — pinning the tag object would
	// give a SHA no checkout of the working tree ever produces.
	commits := map[string]plumbing.Hash{}
	for _, ref := range refs {
		name := ref.Name().String()
		if !strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		tag := strings.TrimPrefix(name, "refs/tags/")
		if peeled := strings.TrimSuffix(tag, "^{}"); peeled != tag {
			commits[peeled] = ref.Hash()
			continue
		}
		if _, ok := commits[tag]; !ok {
			commits[tag] = ref.Hash()
		}
	}
	if len(commits) == 0 {
		return "", plumbing.ZeroHash, fmt.Errorf("%s has no tags", repo)
	}

	names := make([]string, 0, len(commits))
	for tag := range commits {
		names = append(names, tag)
	}
	tag, err := Selection{
		Strategy: w.Select, Constraint: w.Constraint, Allow: w.Allow, Ignore: w.Ignore,
	}.Pick(names)
	if err != nil {
		return "", plumbing.ZeroHash, err
	}
	return tag, commits[tag], nil
}

// newestTouching walks back from head to the newest commit that changed
// something the watch cares about.
//
// **It walks back rather than refusing.** The field's promise is that a commit
// touching nothing in `paths` produces no Bundle, and returning the last commit
// that did touch them keeps that promise: the resolved SHA does not move, so
// nothing new is emitted. Refusing outright would keep it too, but it would
// also mean a Beacon added to a repository whose head happens to be unrelated
// resolves to nothing at all — and stays that way until someone commits in
// those paths. A watch has to be able to describe what is there now, not only
// what changed since it was created.
func (r *Resolver) newestTouching(
	ctx context.Context, w v1alpha1.GitWatch, auth transport.AuthMethod, head plumbing.Hash,
) (string, string, error) {
	repo, err := gogit.CloneContext(ctx, memory.NewStorage(), nil, &gogit.CloneOptions{
		URL: w.Repo, Auth: auth, Depth: historyLimit, NoCheckout: true,
	})
	if err != nil {
		return "", "", fmt.Errorf("reading the history of %s: %w", w.Repo, err)
	}

	commit, err := repo.CommitObject(head)
	if err != nil {
		return "", "", fmt.Errorf("reading commit %s of %s: %w", head, w.Repo, err)
	}

	for range historyLimit {
		touched, err := changedFiles(commit)
		if err != nil {
			return "", "", err
		}
		if matchesAny(touched, w.Paths, w.IgnorePaths) {
			return commit.Hash.String(), subject(commit), nil
		}
		if commit.NumParents() == 0 {
			break
		}
		// First parent only: on a merge, the second parent's changes arrived
		// through the merge commit itself, so following both would count the
		// same work twice and walk into unrelated branches.
		commit, err = commit.Parent(0)
		if err != nil {
			// A shallow clone ends here rather than at a root commit, and that
			// is an exhausted window, not a broken repository.
			break
		}
	}
	return "", "", &ErrNoMatch{Reason: fmt.Sprintf(
		"no commit in the last %d on %s touched %s",
		historyLimit, w.Repo, strings.Join(w.Paths, ", "))}
}

// changedFiles lists what a commit changed, against its first parent. A root
// commit changed everything in it.
func changedFiles(commit *object.Commit) ([]string, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("reading the tree of %s: %w", commit.Hash, err)
	}

	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err == nil {
			if parentTree, err = parent.Tree(); err != nil {
				return nil, fmt.Errorf("reading the tree of %s: %w", parent.Hash, err)
			}
		}
	}
	if parentTree == nil {
		var files []string
		err := tree.Files().ForEach(func(f *object.File) error {
			files = append(files, f.Name)
			return nil
		})
		return files, err
	}

	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return nil, fmt.Errorf("diffing %s: %w", commit.Hash, err)
	}
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		// A rename shows as a delete and an add, so both names count: moving a
		// file out of a watched path is a change to that path.
		if c.From.Name != "" {
			files = append(files, c.From.Name)
		}
		if c.To.Name != "" && c.To.Name != c.From.Name {
			files = append(files, c.To.Name)
		}
	}
	return files, nil
}

// matchesAny reports whether any changed file is inside paths and outside
// ignorePaths.
//
// Prefix semantics rather than globs: `apps/checkout` means that directory and
// everything under it, which is what a monorepo layout needs and what people
// write. Empty paths means everything is included, so ignorePaths alone works.
func matchesAny(files, paths, ignorePaths []string) bool {
	for _, f := range files {
		if len(paths) > 0 && !under(f, paths) {
			continue
		}
		// Applied after paths, so a narrow ignore can carve out of a broad
		// watch — `apps/` minus `apps/*/README.md` in spirit.
		if under(f, ignorePaths) {
			continue
		}
		return true
	}
	return false
}

func under(file string, paths []string) bool {
	for _, p := range paths {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if file == p || strings.HasPrefix(file, p+"/") {
			return true
		}
	}
	return false
}

func subject(commit *object.Commit) string {
	line, _, _ := strings.Cut(commit.Message, "\n")
	return strings.TrimSpace(line)
}
