package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/githubapp"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

// StepGitPullRequest is the value used in `steps[].uses`.
const StepGitPullRequest = "git-pull-request"

// Failure reasons this step can report.
const (
	// ReasonProviderAuthFailed means the host rejected the token.
	ReasonProviderAuthFailed = "ProviderAuthFailed"
	// ReasonProviderFailed is any other refusal from the host's API.
	ReasonProviderFailed = "ProviderFailed"
	// ReasonPullRequestClosed means a human closed the change without merging
	// it. The crossing is over, and no amount of waiting changes that.
	ReasonPullRequestClosed = "PullRequestClosed"
)

// defaultPollInterval is how often an unmerged pull request is re-read.
//
// A review takes hours or days, so this is not a latency budget — it is the
// cost of not having webhooks yet (#102). One request a minute per open
// crossing is far below any host's rate limit.
const defaultPollInterval = time.Minute

// GitPullRequestConfig is the `with:` block of a git-pull-request step.
type GitPullRequestConfig struct {
	// Path is the checkout to read the repository and base branch from,
	// relative to the Passage work dir.
	Path string `json:"path,omitempty"`
	// Repo overrides the repository URL. Empty reads it from the checkout's
	// origin remote, which is where git-clone put it.
	Repo string `json:"repo,omitempty"`
	// Provider is `github` or `gitlab`. Empty is inferred from the host, which
	// only works for the public hosts — an appliance must say which it is.
	Provider string `json:"provider,omitempty"`
	// BaseURL is the API root, for GitHub Enterprise Server and self-managed
	// GitLab.
	BaseURL string `json:"baseURL,omitempty"`
	// Head is the branch to merge. Empty uses the branch git-push creates with
	// `toNewBranch`, so the common flow needs no wiring.
	Head string `json:"head,omitempty"`
	// Base is the branch to merge into. Empty uses the checked-out branch,
	// which is the one git-clone was pointed at.
	Base string `json:"base,omitempty"`

	// Title is the pull request's title. Empty is generated from the Passage.
	Title string `json:"title,omitempty"`
	// Body is the description. Empty is generated from what is being promoted.
	Body string `json:"body,omitempty"`
	// Labels are applied on creation. A label added later is not removed.
	Labels []string `json:"labels,omitempty"`

	// WaitForMerge holds the Passage open until the pull request merges.
	//
	// A pointer so that not saying anything means true. A crossing that
	// succeeds the moment a pull request opens would mark the Bundle as having
	// cleared the environment before any human looked at it, which is the
	// opposite of what asking for review means.
	WaitForMerge *bool `json:"waitForMerge,omitempty"`
	// PollInterval overrides how often an open pull request is re-read.
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// CredentialsRef names a Secret holding an API token, or a GitHub App's
	// keys. Opening a pull request needs API access, which push access alone
	// does not give.
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// GitPullRequest opens a pull request and, by default, waits for it to merge.
//
// The waiting is the point. A review takes as long as it takes, so the step
// reports Running and returns rather than blocking — which is what the polling
// engine exists for. The wait survives a controller restart because it lives in
// the Passage's status, not in a goroutine.
type GitPullRequest struct {
	client client.Client
	// providers is injectable so tests can point the step at a fake host
	// without a network.
	providers func(provider.Kind, provider.Config) (provider.Provider, error)
}

// NewGitPullRequest returns a git-pull-request step.
func NewGitPullRequest(c client.Client) *GitPullRequest {
	return &GitPullRequest{client: c, providers: provider.New}
}

// Name implements passage.Runner.
func (g *GitPullRequest) Name() string { return StepGitPullRequest }

// Run implements passage.Runner.
func (g *GitPullRequest) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[GitPullRequestConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPullRequest, err)
	}

	spec, err := g.resolveSpec(sc, cfg)
	if err != nil {
		return passage.StepResult{}, err
	}

	token, err := g.token(ctx, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, err
	}

	kind := provider.Kind(cfg.Provider)
	if kind == "" {
		kind = provider.KindFor(spec.Repo.Host)
	}
	host, err := g.providers(kind, provider.Config{BaseURL: cfg.BaseURL, Token: token})
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPullRequest, err)
	}

	pr, err := host.EnsurePullRequest(ctx, spec)
	if err != nil {
		return passage.StepResult{}, providerError("opening a pull request", err)
	}

	output := map[string]any{
		"number": pr.Number, "url": pr.URL, "state": string(pr.State), "branch": pr.Head,
	}

	if cfg.WaitForMerge != nil && !*cfg.WaitForMerge {
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("opened %s", pr.URL),
			Output:  output,
		}, nil
	}

	// Re-read rather than trust what opening returned: on a later attempt the
	// pull request has been open for a while and its state is the whole point.
	pr, err = host.PullRequest(ctx, spec.Repo, pr.Number)
	if err != nil {
		return passage.StepResult{}, providerError("reading a pull request", err)
	}
	output["state"] = string(pr.State)

	switch pr.State {
	case provider.Merged:
		// The merge commit, not the branch's: a squashing host lands the change
		// under a hash that never existed locally, and that is what a later
		// flux-wait has to wait for.
		output["sha"] = pr.MergeCommit
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("%s merged as %s", pr.URL, short(pr.MergeCommit)),
			Output:  output,
		}, nil

	case provider.Closed:
		return passage.StepResult{Output: output}, passage.FailTerminal(ReasonPullRequestClosed,
			"%s was closed without merging — the crossing was refused by whoever closed it", pr.URL)

	default:
		interval := defaultPollInterval
		if cfg.PollInterval != nil && cfg.PollInterval.Duration > 0 {
			interval = cfg.PollInterval.Duration
		}
		return passage.StepResult{
			Phase:      v1alpha1.StepRunning,
			Message:    fmt.Sprintf("waiting for %s to merge", pr.URL),
			Output:     output,
			RetryAfter: interval,
		}, nil
	}
}

// resolveSpec fills in what the author did not have to say: the repository and
// base branch come from the checkout, and the head branch from the convention
// git-push uses for `toNewBranch`.
func (g *GitPullRequest) resolveSpec(
	sc *passage.StepContext, cfg GitPullRequestConfig,
) (provider.PullRequestSpec, error) {
	spec := provider.PullRequestSpec{
		Head: cfg.Head, Base: cfg.Base, Title: cfg.Title, Body: cfg.Body, Labels: cfg.Labels,
	}
	if spec.Head == "" {
		spec.Head = "hecate/" + sc.Passage
	}
	if spec.Title == "" {
		spec.Title = fmt.Sprintf("Promote %s to %s", bundleName(sc), sc.Gate)
	}

	cloneURL := cfg.Repo
	if cloneURL == "" || spec.Base == "" {
		dir, err := checkoutPath(sc.WorkDir, cfg.Path)
		if err != nil {
			return spec, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPullRequest, err)
		}
		repo, err := openRepo(dir)
		if err != nil {
			return spec, err
		}
		if cloneURL == "" {
			remote, err := repo.Remote(git.DefaultRemoteName)
			if err != nil || len(remote.Config().URLs) == 0 {
				return spec, passage.FailTerminal(ReasonInvalidConfig,
					"%s: the checkout has no origin remote, so set repo:", StepGitPullRequest)
			}
			cloneURL = remote.Config().URLs[0]
		}
		if spec.Base == "" {
			head, err := repo.Head()
			if err != nil {
				return spec, passage.FailTerminal(ReasonGitFailed,
					"%s: reading the checked-out branch: %s", StepGitPullRequest, err)
			}
			spec.Base = head.Name().Short()
		}
	}

	parsed, err := provider.ParseRepo(cloneURL)
	if err != nil {
		return spec, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepGitPullRequest, err)
	}
	spec.Repo = parsed
	return spec, nil
}

// token reads the host credential. The key is `token`, with `password`
// accepted so one Secret can serve both git and the API.
func (g *GitPullRequest) token(
	ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef,
) (string, error) {
	if ref == nil {
		return "", passage.FailTerminal(ReasonInvalidConfig,
			"%s: credentialsRef is required — opening a pull request needs an API token, "+
				"which is not the same thing as push access", StepGitPullRequest)
	}
	return apiToken(ctx, g.client, namespace, ref, StepGitPullRequest)
}

// apiToken reads a host API token from a Secret, shared by every step that
// talks to a git host's API.
//
// `token` wins over `password` so one Secret can carry both push credentials
// and an API token — which is the common case, since they are frequently the
// same string on GitHub and frequently not on GitLab.
func apiToken(
	ctx context.Context, c client.Client, namespace string,
	ref *v1alpha1.LocalSecretRef, step string,
) (string, error) {
	if c == nil {
		return "", passage.FailTerminal(ReasonInvalidConfig,
			"%s: credentialsRef set but the step has no client", step)
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", passage.FailTerminal(ReasonInvalidConfig,
			"%s: reading Secret %s/%s: %s", step, namespace, ref.Name, err)
	}
	// A GitHub App credential first, because it is the better one and because a
	// Secret carrying both is carrying a fallback nobody meant to rely on.
	//
	// The token it mints works as an API token *and* as a git password, so the
	// git steps that push resolve the same Secret to the same credential —
	// which is the point. Two credential paths, one short-lived and one not,
	// would leave the permanent one in place and change nothing (#118).
	if githubapp.HasAppKeys(secret.Data) {
		token, err := appToken(ctx, secret.Data, step)
		if err != nil {
			return "", err
		}
		return token, nil
	}

	for _, key := range []string{"token", "password"} {
		if v := string(secret.Data[key]); v != "" {
			return v, nil
		}
	}
	return "", passage.FailTerminal(ReasonInvalidConfig,
		"%s: Secret %s has no token, password or GitHub App key", step, ref.Name)
}

// appToken mints an installation token from a GitHub App Secret.
//
// Sources are cached per Secret content: a Passage's steps each resolve
// credentials independently, and minting a token per step would turn one
// promotion into several API calls against a rate limit that is shared with
// everything else the App does.
func appToken(ctx context.Context, data map[string][]byte, step string) (string, error) {
	creds, err := githubapp.FromSecret(data, string(data["baseURL"]))
	if err != nil {
		return "", passage.FailTerminal(ReasonInvalidConfig, "%s: %s", step, err)
	}
	src, err := githubapp.SourceFor(creds)
	if err != nil {
		return "", passage.FailTerminal(ReasonInvalidConfig, "%s: %s", step, err)
	}
	token, err := src.Token(ctx)
	if err != nil {
		// Not terminal: an installation token is minted over the network, and a
		// GitHub that is briefly unavailable is worth retrying rather than
		// failing the crossing over.
		return "", fmt.Errorf("%s: %w", step, err)
	}
	return token, nil
}

// providerError classifies a host's refusal. A bad token will not become good
// by retrying; a 500 or a timeout might.
func providerError(what string, err error) error {
	switch {
	case provider.IsAuth(err):
		return passage.FailTerminal(ReasonProviderAuthFailed, "%s: %s", what, err)
	case provider.IsNotFound(err):
		return passage.FailTerminal(ReasonProviderFailed,
			"%s: %s — check the repository name and that the token can see it", what, err)
	default:
		return passage.Fail(ReasonProviderFailed, "%s: %s", what, err)
	}
}

func bundleName(sc *passage.StepContext) string {
	if sc.Bundle == nil {
		return "a Bundle"
	}
	if sc.Bundle.Spec.Alias != "" {
		return sc.Bundle.Spec.Alias
	}
	return sc.Bundle.Name
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
