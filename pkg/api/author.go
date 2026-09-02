package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	hgit "github.com/olafkfreund/hecate/pkg/git"
	"github.com/olafkfreund/hecate/pkg/githubapp"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
	"github.com/olafkfreund/hecate/pkg/safepath"
)

// composedStep is one row of the browser's step list — the same shape as
// stage 1's `ComposedStep` (ui/lib/yaml.ts), sent as JSON instead of rendered
// to YAML by the browser (D59).
type composedStep struct {
	Uses string          `json:"uses"`
	As   string          `json:"as,omitempty"`
	With json.RawMessage `json:"with,omitempty"`
}

// AuthorPassageRequest is a whole Passage manifest plus where to open a pull
// request for it. Exported so cmd/apishape can check ui/lib/api.ts's copy
// against the fields this endpoint actually reads (D62).
type AuthorPassageRequest struct {
	// Name is the Passage's metadata.name. Applying two authored Passages with
	// the same name would collide, so this has no default — the author names
	// it, the same as writing the YAML by hand would require.
	Name string `json:"name"`
	// Gate is the Gate this Passage crosses. api.gates() already lists every
	// Gate the caller can see, for a form to populate a picker from rather
	// than asking someone to type an identifier by hand.
	Gate string `json:"gate"`
	// Bundle is the Bundle this Passage moves. api.bundles() lists it the same
	// way Gate does.
	Bundle string `json:"bundle"`
	// Steps is the ordered step list, exactly as PassageSpec.Steps.
	Steps []composedStep `json:"steps"`

	// Repo is the repository's clone URL. Not derived: a Gate's git-clone step
	// names the *fleet* repository it writes application manifests into, which
	// is not the same repository as the one holding the Gate's own YAML, and
	// Flux ownership is not reliably present either (D58).
	Repo string `json:"repo"`
	// Path is the file this pull request writes, relative to the repository
	// root — a whole Passage manifest, its own file. Refused when it already
	// exists in the base branch unless Overwrite is set (D58).
	Path string `json:"path"`
	// Overwrite allows replacing a file that already exists at Path. Off by
	// default: a wrong or reused Path would otherwise silently destroy
	// whatever was already committed there.
	Overwrite bool `json:"overwrite,omitempty"`
	// Base is the branch to open the pull request against. Empty uses the
	// repository's default branch.
	Base string `json:"base,omitempty"`
	// Head names the branch this pull request is opened from. Empty derives
	// one from Path.
	Head string `json:"head,omitempty"`

	// Provider is "github" or "gitlab". Empty is inferred from Repo's host,
	// which only works for the public hosts.
	Provider string `json:"provider,omitempty"`
	// BaseURL is the API root, for GitHub Enterprise Server and self-managed
	// GitLab.
	BaseURL string `json:"baseURL,omitempty"`

	Title  string   `json:"title,omitempty"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`

	// CredentialsRef names a Secret in this namespace holding push access and
	// an API token — the same resolution git-pull-request and git-push use, an
	// App key over a static one (#118).
	CredentialsRef string `json:"credentialsRef"`
}

// AuthoredPullRequest is what opening one returns. Exported, and a real
// struct rather than the map[string]any this endpoint used to build by hand
// — a renamed field in that map compiled, passed apishape (because nothing
// there could see it), and reached the browser as `undefined` the way
// Explanation.reason/remedy and GateOccupant.since both did (D62).
type AuthoredPullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Branch string `json:"branch"`
	Repo   string `json:"repo"`
}

// pathExistsError refuses to overwrite a file that already exists on the
// target branch — see D58.
type pathExistsError struct{ Path string }

func (e *pathExistsError) Error() string {
	return fmt.Sprintf(
		"%s already exists on the target branch — refusing to overwrite it; pass overwrite:true if that is intended",
		e.Path)
}

// stepProblemsError refuses to open a pull request for a step list that would
// not admit — see passage.Registry.Validate.
type stepProblemsError struct {
	Problems []passage.StepProblem
}

func (e *stepProblemsError) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = p.Error()
	}
	return "the step list would not admit: " + strings.Join(msgs, "; ")
}

// StepProblem is the JSON shape a form can point a user at: which step, and
// why. Mirrors passage.StepProblem, whose Err is a Go error and so cannot be
// serialised directly — this is that type's wire shape, exported for
// cmd/apishape rather than left as the map[string]any dto() used to build,
// which apishape could not see (D62).
type StepProblem struct {
	Index int `json:"index"`
	// No omitempty: the map[string]any this replaced always sent "uses", even
	// empty, and the wire shape must not change underneath it (D62).
	Uses    string `json:"uses"`
	Message string `json:"message"`
}

// dto is the JSON shape a form can point a user at: which step, and why.
func (e *stepProblemsError) dto() []StepProblem {
	out := make([]StepProblem, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = StepProblem{Index: p.Index, Uses: p.Uses, Message: p.Err.Error()}
	}
	return out
}

// authorPassage renders a whole Passage manifest, commits it to a new branch
// of the named repository, and opens a pull request through pkg/provider.
//
// This never writes to the cluster — no Create, Apply or Update on any Hecate
// CRD. The cluster stays git's (hecate#172, and see D58/D59 for why the
// target is an explicit repo/path rather than derived, why the manifest is
// its own file written wholesale rather than merged, and why the YAML is
// rendered here rather than trusted from the browser).
func (s *Server) authorPassage(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	namespace := r.PathValue("namespace")

	var req AuthorPassageRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, &BadRequest{Reason: "name is required — it becomes the Passage's metadata.name"}
	}
	if strings.TrimSpace(req.Gate) == "" {
		return nil, &BadRequest{Reason: "gate is required"}
	}
	if strings.TrimSpace(req.Bundle) == "" {
		return nil, &BadRequest{Reason: "bundle is required"}
	}
	if len(req.Steps) == 0 {
		return nil, &BadRequest{Reason: "steps is required — there is nothing to author"}
	}
	if strings.TrimSpace(req.Repo) == "" {
		return nil, &BadRequest{Reason: "repo is required — Hecate cannot derive which repository holds this Gate's YAML (D58)"}
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, &BadRequest{Reason: "path is required"}
	}
	if strings.TrimSpace(req.CredentialsRef) == "" {
		return nil, &BadRequest{Reason: "credentialsRef is required — opening a pull request needs an API token, which push access alone does not give"}
	}

	steps := toSteps(req.Steps)

	// Refuse the same things admission would, before anything is written
	// anywhere (hecate#172 scope item 4).
	if s.Steps != nil {
		if problems := s.Steps.Validate(steps); len(problems) > 0 {
			return nil, &stepProblemsError{Problems: problems}
		}
	}

	manifest := &v1alpha1.Passage{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Passage"},
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: namespace},
		Spec:       v1alpha1.PassageSpec{Gate: req.Gate, Bundle: req.Bundle, Steps: steps},
	}
	rendered, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("rendering YAML: %w", err)
	}

	gitAuth, apiToken, err := s.resolveAuthorCredentials(ctx, namespace, req.CredentialsRef)
	if err != nil {
		return nil, &BadRequest{Reason: err.Error()}
	}

	repo, err := provider.ParseRepo(req.Repo)
	if err != nil {
		return nil, &BadRequest{Reason: err.Error()}
	}
	kind := provider.Kind(req.Provider)
	if kind == "" {
		kind = provider.KindFor(repo.Host)
	}

	head := req.Head
	if head == "" {
		head = "hecate/author-" + slug(req.Path)
	}

	publish := s.publish
	if publish == nil {
		publish = gitPublish
	}
	base, err := publish(ctx, publishRequest{
		CloneURL: req.Repo, Base: req.Base, Head: head, Path: req.Path, Content: rendered,
		Auth: gitAuth, Overwrite: req.Overwrite,
	})
	if err != nil {
		return nil, fmt.Errorf("committing %s: %w", req.Path, err)
	}

	providers := s.providers
	if providers == nil {
		providers = provider.New
	}
	host, err := providers(kind, provider.Config{BaseURL: req.BaseURL, Token: apiToken})
	if err != nil {
		return nil, &BadRequest{Reason: err.Error()}
	}

	title := req.Title
	if title == "" {
		title = "Author a Passage: " + req.Path
	}
	body := req.Body
	if body == "" {
		body = fmt.Sprintf(
			"Opened by %s through Hecate's authoring UI (hecate#172).\n\n```yaml\n%s```\n",
			subject.Name, rendered)
	}

	pr, err := host.EnsurePullRequest(ctx, provider.PullRequestSpec{
		Repo: repo, Head: head, Base: base, Title: title, Body: body, Labels: req.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("opening a pull request: %w", err)
	}

	return &AuthoredPullRequest{
		Number: pr.Number, URL: pr.URL, State: string(pr.State),
		Branch: pr.Head, Repo: repo.String(),
	}, nil
}

// resolveAuthorCredentials reads one Secret for both a git transport auth
// method and a raw API token, because opening a pull request needs both.
// Precedence matches git-pull-request and git-push: an App key wins (#118).
//
// ponytail: a static credential is always treated as a token — the same
// value serves as the git password and the API bearer, which is what GitHub
// and GitLab both accept over HTTPS with any username. An SSH `identity` key
// pushes fine on its own (git-clone/git-push already support one), but this
// endpoint has no separate field for "the API token when push is over SSH";
// add one if a fleet that pushes over SSH needs authoring too.
func (s *Server) resolveAuthorCredentials(
	ctx context.Context, namespace, name string,
) (transport.AuthMethod, string, error) {
	var secret corev1.Secret
	if err := s.Ops.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, "", fmt.Errorf("reading Secret %s/%s: %w", namespace, name, err)
	}

	if githubapp.HasAppKeys(secret.Data) {
		creds, err := githubapp.FromSecret(secret.Data, string(secret.Data["baseURL"]))
		if err != nil {
			return nil, "", fmt.Errorf("secret %s/%s: %w", namespace, name, err)
		}
		src, err := githubapp.SourceFor(creds)
		if err != nil {
			return nil, "", fmt.Errorf("secret %s/%s: %w", namespace, name, err)
		}
		token, err := src.Token(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("minting an installation token: %w", err)
		}
		return hgit.TokenAuth(token), token, nil
	}

	for _, key := range []string{"token", "password"} {
		if v := string(secret.Data[key]); v != "" {
			return hgit.TokenAuth(v), v, nil
		}
	}
	return nil, "", fmt.Errorf("secret %s has no token, password or GitHub App key", name)
}

// toSteps turns the browser's step list into the type Registry.Validate and
// the YAML renderer both read, so there is exactly one shape describing what
// a step is.
func toSteps(in []composedStep) []v1alpha1.Step {
	out := make([]v1alpha1.Step, len(in))
	for i, s := range in {
		step := v1alpha1.Step{Uses: s.Uses, As: s.As}
		if len(s.With) > 0 {
			step.With = &apiextensionsv1.JSON{Raw: append([]byte(nil), s.With...)}
		}
		out[i] = step
	}
	return out
}

// publishRequest is what gitPublish needs to commit one file to a new branch.
type publishRequest struct {
	CloneURL string
	Base     string
	Head     string
	Path     string
	Content  []byte
	Auth     transport.AuthMethod
	// Overwrite allows replacing a file that already exists at Path. Off by
	// default — see pathExistsError and D58.
	Overwrite bool
}

// gitPublish clones the repository, writes and commits one file, and pushes
// it to a new branch — the "commit to a branch" half of hecate#172 scope item
// 3. Returns the base branch actually used, for the pull request that follows.
//
// A fresh temporary checkout per request, unlike the Passage steps' reused
// work dir: there is no Passage here to be re-entrant for — this runs once
// per click and the tree is gone before the handler returns.
func gitPublish(ctx context.Context, req publishRequest) (base string, err error) {
	dir, err := os.MkdirTemp("", "hecate-author-*")
	if err != nil {
		return "", fmt.Errorf("creating a scratch checkout: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	opts := &git.CloneOptions{URL: req.CloneURL, Auth: req.Auth, Depth: 1}
	if req.Base != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(req.Base)
		opts.SingleBranch = true
	}
	repo, err := git.PlainCloneContext(ctx, dir, false, opts)
	if err != nil {
		return "", fmt.Errorf("cloning %s: %w", req.CloneURL, err)
	}
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("reading the checked-out branch: %w", err)
	}
	base = req.Base
	if base == "" {
		base = head.Name().Short()
	}

	// safepath.Join does the containment work, shared with the Passage steps'
	// checkoutPath: `repo` and `path` come from the same HTTP caller here, so
	// a repository can be cloned with a symlink already committed into it
	// (say `apps -> /etc`), and a lexical check alone would clear
	// `dir/apps/passage.yaml` while the filesystem resolves it elsewhere
	// entirely. Join returns the path actually proven contained, so that is
	// what Lstat/MkdirAll/WriteFile below use — not a second, unchecked join
	// of the same tainted string.
	safe, err := safepath.Join(dir, req.Path)
	if err != nil {
		return "", fmt.Errorf("path %q: %w", req.Path, err)
	}

	// A Passage manifest is its own file (D58): writing it wholesale is
	// correct, but only because nothing else is expected to live there. A
	// Path that already holds something — a reused name, or a typo landing on
	// an existing Gate's manifest — must not be silently destroyed.
	//
	// Lstat, not Stat, and checked before the exists/Overwrite decision: a
	// symlink left at Path by the same commit that could plant one in a
	// parent directory is never "the file we committed" to replace, only a
	// way to write through it to wherever it points — refused outright, with
	// no Overwrite escape hatch for it. There is no TOCTOU window worth
	// closing with O_NOFOLLOW here: this checkout is a fresh temp clone this
	// request alone can see, so nothing else can swap the symlink in between
	// this check and the write below.
	switch info, err := os.Lstat(safe); {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return "", fmt.Errorf("path %q is a symlink — refusing to write through it", req.Path)
	case err == nil:
		if !req.Overwrite {
			return "", &pathExistsError{Path: req.Path}
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("checking %s: %w", req.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(safe), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(req.Path), err)
	}
	if err := os.WriteFile(safe, req.Content, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", req.Path, err)
	}

	tree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("opening the worktree: %w", err)
	}
	if _, err := tree.Add(filepath.ToSlash(req.Path)); err != nil {
		return "", fmt.Errorf("staging %s: %w", req.Path, err)
	}
	sig := &object.Signature{Name: "Hecate", Email: "hecate@users.noreply.github.com", When: time.Now().UTC()}
	if _, err := tree.Commit(fmt.Sprintf("author: %s", req.Path), &git.CommitOptions{
		Author: sig, Committer: sig,
	}); err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}

	spec := config.RefSpec(fmt.Sprintf("%s:%s", head.Name(), plumbing.NewBranchReferenceName(req.Head)))
	if err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: git.DefaultRemoteName, RefSpecs: []config.RefSpec{spec}, Auth: req.Auth,
	}); err != nil {
		return "", fmt.Errorf("pushing %s: %w", req.Head, err)
	}

	return base, nil
}

// slug turns a file path into a branch-safe name, so the default head branch
// needs no wiring: "demo/pipeline.yaml" becomes "pipeline".
func slug(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "passage"
	}
	return out
}
