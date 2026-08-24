package steps

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

// StepCommitStatus is the value used in `steps[].uses`.
const StepCommitStatus = "commit-status"

// CommitStatusConfig is the `with:` block of a commit-status step.
type CommitStatusConfig struct {
	// SHA is the commit to report on. Empty reads HEAD from the checkout,
	// which is the commit git-commit made and git-push published.
	SHA string `json:"sha,omitempty"`
	// Path is the checkout to read the repository and HEAD from, relative to
	// the Passage work dir.
	Path string `json:"path,omitempty"`
	// Repo overrides the repository URL. Empty reads it from the checkout's
	// origin remote.
	Repo string `json:"repo,omitempty"`
	// Provider is `github` or `gitlab`. Empty is inferred from the host.
	Provider string `json:"provider,omitempty"`
	// BaseURL is the API root, for GitHub Enterprise Server and self-managed
	// GitLab.
	BaseURL string `json:"baseURL,omitempty"`
	// Context names the check. Empty is `hecate/<gate>`, so two Gates reporting
	// on the same commit do not overwrite each other.
	Context string `json:"context,omitempty"`
	// State overrides the outcome. Empty reports what actually happened, which
	// is almost always what you want; set `Pending` on a step at the top of a
	// Passage to show the crossing has started.
	State string `json:"state,omitempty"`
	// Description overrides the one-line summary.
	Description string `json:"description,omitempty"`
	// TargetURL is where a human goes for detail.
	TargetURL string `json:"targetURL,omitempty"`

	// CredentialsRef names a Secret holding an API token, or a GitHub App's
	// keys. Required: reporting a status needs API access, which is not the
	// same thing as push access.
	CredentialsRef *v1alpha1.LocalSecretRef `json:"credentialsRef,omitempty"`
}

// CommitStatus reports a crossing's outcome against the commit Flux applied.
//
// **It is meant to be run with `if: always`.** Placed on the happy path it can
// only ever report success, which is worse than reporting nothing: a check that
// is green whenever it appears looks like coverage and is not. The engine tells
// the step whether the Passage failed (D46), so the state is the real one
// rather than something the author has to keep in step with the rest of the
// Passage.
//
// What it reports on is the commit **Hecate made and Flux applied** — not the
// application commit that produced the image. Hecate resolves images to
// digests and never learns which source commit built one, so claiming
// otherwise would be a check with a true-looking name and no basis.
type CommitStatus struct {
	client    client.Client
	providers func(provider.Kind, provider.Config) (provider.Provider, error)
}

// NewCommitStatus returns a commit-status step.
func NewCommitStatus(c client.Client) *CommitStatus {
	return &CommitStatus{client: c, providers: provider.New}
}

// Name implements passage.Runner.
func (s *CommitStatus) Name() string { return StepCommitStatus }

// CheckConfig implements passage.ConfigChecker, so a Gate is refused at
// admission rather than part-way through a crossing.
func (s *CommitStatus) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[CommitStatusConfig](raw)
	if err != nil {
		return err
	}
	if _, err := state(cfg.State, false); err != nil {
		return err
	}
	return nil
}

// Run implements passage.Runner.
func (s *CommitStatus) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[CommitStatusConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepCommitStatus, err)
	}

	reported, err := state(cfg.State, sc.Failed)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepCommitStatus, err)
	}

	sha, cloneURL, err := s.locate(sc, cfg)
	if err != nil {
		return passage.StepResult{}, err
	}

	repo, err := provider.ParseRepo(cloneURL)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepCommitStatus, err)
	}

	kind := provider.Kind(cfg.Provider)
	if kind == "" {
		kind = provider.KindFor(repo.Host)
	}

	token, err := s.token(ctx, sc.Namespace, cfg.CredentialsRef)
	if err != nil {
		return passage.StepResult{}, err
	}

	host, err := s.providers(kind, provider.Config{BaseURL: cfg.BaseURL, Token: token})
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepCommitStatus, err)
	}

	status := provider.CommitStatus{
		Repo:        repo,
		SHA:         sha,
		State:       reported,
		Context:     firstNonEmpty(cfg.Context, "hecate/"+sc.Gate),
		Description: firstNonEmpty(cfg.Description, describeOutcome(sc)),
		TargetURL:   cfg.TargetURL,
	}
	if err := host.SetCommitStatus(ctx, status); err != nil {
		return passage.StepResult{}, providerError("reporting a commit status", err)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("reported %s on %s", reported, short(sha)),
		Output: map[string]any{
			"state": string(reported), "sha": sha, "context": status.Context,
		},
	}, nil
}

// locate resolves the commit and repository, from configuration or from the
// checkout an earlier step left behind.
func (s *CommitStatus) locate(
	sc *passage.StepContext, cfg CommitStatusConfig,
) (sha, cloneURL string, err error) {
	sha, cloneURL = cfg.SHA, cfg.Repo
	if sha != "" && cloneURL != "" {
		return sha, cloneURL, nil
	}

	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return "", "", passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepCommitStatus, err)
	}
	repo, err := openRepo(dir)
	if err != nil {
		// The work dir is disposable (D19), so this is what a controller
		// restart mid-crossing looks like. Say which of the two things is
		// missing rather than reporting a bare git error.
		return "", "", passage.FailTerminal(ReasonWorkDirLost,
			"%s: no checkout to read the commit from — set sha and repo explicitly "+
				"if this step runs without one: %s", StepCommitStatus, err)
	}
	if sha == "" {
		head, headErr := repo.Head()
		if headErr != nil {
			return "", "", passage.FailTerminal(ReasonGitFailed, "%s: %s", StepCommitStatus, headErr)
		}
		sha = head.Hash().String()
	}
	if cloneURL == "" {
		remote, remoteErr := repo.Remote("origin")
		if remoteErr != nil || len(remote.Config().URLs) == 0 {
			return "", "", passage.FailTerminal(ReasonInvalidConfig,
				"%s: the checkout has no origin remote; set repo explicitly", StepCommitStatus)
		}
		cloneURL = remote.Config().URLs[0]
	}
	return sha, cloneURL, nil
}

func (s *CommitStatus) token(
	ctx context.Context, namespace string, ref *v1alpha1.LocalSecretRef,
) (string, error) {
	if ref == nil {
		return "", passage.FailTerminal(ReasonInvalidConfig,
			"%s: credentialsRef is required — reporting a status needs an API token, "+
				"which is not the same thing as push access", StepCommitStatus)
	}
	return apiToken(ctx, s.client, namespace, ref, StepCommitStatus)
}

// state resolves what to report: the configured override, or what actually
// happened.
func state(configured string, failed bool) (provider.CommitState, error) {
	switch provider.CommitState(configured) {
	case "":
		if failed {
			return provider.StateFailure, nil
		}
		return provider.StateSuccess, nil
	case provider.StatePending:
		return provider.StatePending, nil
	case provider.StateSuccess:
		return provider.StateSuccess, nil
	case provider.StateFailure:
		return provider.StateFailure, nil
	default:
		return "", fmt.Errorf("unknown state %q: use Pending, Success or Failure, "+
			"or leave it unset to report what happened", configured)
	}
}

// describeOutcome is the default one-liner beside the check.
func describeOutcome(sc *passage.StepContext) string {
	if sc.Failed {
		return fmt.Sprintf("%s did not cross %s", bundleName(sc), sc.Gate)
	}
	return fmt.Sprintf("%s crossed %s", bundleName(sc), sc.Gate)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
