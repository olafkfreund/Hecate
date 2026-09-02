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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	hgit "github.com/olafkfreund/hecate/pkg/git"
	"github.com/olafkfreund/hecate/pkg/githubapp"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

// composedStep is one row of the browser's step list — the same shape as
// stage 1's `ComposedStep` (ui/lib/yaml.ts), sent as JSON instead of rendered
// to YAML by the browser (D59).
type composedStep struct {
	Uses string          `json:"uses"`
	As   string          `json:"as,omitempty"`
	With json.RawMessage `json:"with,omitempty"`
}

// authorPassageRequest is a Passage's step list plus where to open a pull
// request for it.
type authorPassageRequest struct {
	Steps []composedStep `json:"steps"`

	// Repo is the repository's clone URL. Not derived: a Gate's git-clone step
	// names the *fleet* repository it writes application manifests into, which
	// is not the same repository as the one holding the Gate's own YAML, and
	// Flux ownership is not reliably present either (D58).
	Repo string `json:"repo"`
	// Path is the file this pull request writes, relative to the repository
	// root. Replaced wholesale with the rendered steps (D58) — this does not
	// merge into an existing manifest.
	Path string `json:"path"`
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

// dto is the JSON shape a form can point a user at: which step, and why.
func (e *stepProblemsError) dto() []map[string]any {
	out := make([]map[string]any, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = map[string]any{"index": p.Index, "uses": p.Uses, "message": p.Err.Error()}
	}
	return out
}

// authorPassage renders a composed step list to YAML, commits it to a new
// branch of the named repository, and opens a pull request through
// pkg/provider.
//
// This never writes to the cluster — no Create, Apply or Update on any Hecate
// CRD. The cluster stays git's (hecate#172, and see D58/D59 for why the
// target is an explicit repo/path rather than derived, and why the YAML is
// rendered here rather than trusted from the browser).
func (s *Server) authorPassage(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	namespace := r.PathValue("namespace")

	var req authorPassageRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
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

	rendered, err := renderSteps(steps)
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
		CloneURL: req.Repo, Base: req.Base, Head: head, Path: req.Path, Content: rendered, Auth: gitAuth,
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

	return map[string]any{
		"number": pr.Number, "url": pr.URL, "state": string(pr.State),
		"branch": pr.Head, "repo": repo.String(),
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
			return nil, "", fmt.Errorf("Secret %s/%s: %w", namespace, name, err)
		}
		src, err := githubapp.SourceFor(creds)
		if err != nil {
			return nil, "", fmt.Errorf("Secret %s/%s: %w", namespace, name, err)
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
	return nil, "", fmt.Errorf("Secret %s has no token, password or GitHub App key", name)
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

// renderSteps is the `steps:` block of a Gate's `spec.passage`, rendered
// server-side from the exact type Validate checked (D59) — the same output
// shape stage 1's `stepsYAML` produces, but the copy that ends up in git.
func renderSteps(steps []v1alpha1.Step) ([]byte, error) {
	return yaml.Marshal(struct {
		Steps []v1alpha1.Step `json:"steps"`
	}{Steps: steps})
}

// publishRequest is what gitPublish needs to commit one file to a new branch.
type publishRequest struct {
	CloneURL string
	Base     string
	Head     string
	Path     string
	Content  []byte
	Auth     transport.AuthMethod
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

	full := filepath.Join(dir, filepath.FromSlash(req.Path))
	// A `path` of "../../etc" would otherwise write outside the checkout, the
	// same guard the Passage steps' checkoutPath applies.
	if rel, err := filepath.Rel(dir, full); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes the repository", req.Path)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(req.Path), err)
	}
	if err := os.WriteFile(full, req.Content, 0o644); err != nil {
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
