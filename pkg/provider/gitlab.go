package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// gitlabAPI is the public host's API root. Self-managed GitLab serves the same
// API under /api/v4 on its own hostname.
const gitlabAPI = "https://gitlab.com/api/v4"

type gitlabProvider struct{ c *client }

func newGitLab(cfg Config) (Provider, error) {
	if cfg.Token == "" {
		return nil, errors.New("gitlab: no token")
	}
	c, err := newClient(cfg.BaseURL, gitlabAPI, map[string]string{
		// PRIVATE-TOKEN covers personal, project and group access tokens, which
		// is every kind an automation is given.
		"PRIVATE-TOKEN": cfg.Token,
		"Accept":        "application/json",
		"User-Agent":    "hecate",
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: %w", err)
	}
	return &gitlabProvider{c: c}, nil
}

func (g *gitlabProvider) Kind() Kind { return GitLab }

// gitlabMR is the subset of the merge request payload a promotion cares about.
type gitlabMR struct {
	// IID is the per-project number, the one in the URL and the one humans
	// quote. ID is global and means nothing to anybody.
	IID             int    `json:"iid"`
	WebURL          string `json:"web_url"`
	State           string `json:"state"`
	SourceBranch    string `json:"source_branch"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	SquashCommitSHA string `json:"squash_commit_sha"`
}

func (m gitlabMR) convert() *PullRequest {
	pr := &PullRequest{Number: m.IID, URL: m.WebURL, Head: m.SourceBranch}
	switch m.State {
	case "merged":
		pr.State = Merged
		// A squashed merge leaves merge_commit_sha null and puts the commit
		// that actually landed in squash_commit_sha.
		pr.MergeCommit = m.MergeCommitSHA
		if pr.MergeCommit == "" {
			pr.MergeCommit = m.SquashCommitSHA
		}
	case "opened", "locked":
		pr.State = Open
	default:
		pr.State = Closed
	}
	return pr
}

// project is the URL-encoded path GitLab uses to identify a project, so a
// nested group survives the trip.
func project(repo Repo) string { return url.PathEscape(repo.Slug()) }

func (g *gitlabProvider) EnsurePullRequest(ctx context.Context, spec PullRequestSpec) (*PullRequest, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	if existing, err := g.openFor(ctx, spec.Repo, spec.Head); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	body := map[string]any{
		"source_branch": spec.Head,
		"target_branch": spec.Base,
		"title":         spec.Title,
		"description":   spec.Body,
	}
	if len(spec.Labels) > 0 {
		// Set at creation rather than in a second call: GitLab takes them here,
		// so there is no window where the merge request exists unlabelled.
		body["labels"] = strings.Join(spec.Labels, ",")
	}

	var created gitlabMR
	err := g.c.do(ctx, http.MethodPost, "projects/"+project(spec.Repo)+"/merge_requests", body, &created)
	if err != nil {
		// GitLab answers a duplicate source branch with 409. Same reasoning as
		// GitHub's 422: the merge request the step wants now exists.
		if statusIs(err, http.StatusConflict) {
			if existing, lookupErr := g.openFor(ctx, spec.Repo, spec.Head); lookupErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return created.convert(), nil
}

func (g *gitlabProvider) openFor(ctx context.Context, repo Repo, head string) (*PullRequest, error) {
	query := fmt.Sprintf("projects/%s/merge_requests?state=opened&source_branch=%s",
		project(repo), url.QueryEscape(head))

	var found []gitlabMR
	if err := g.c.do(ctx, http.MethodGet, query, nil, &found); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return found[0].convert(), nil
}

func (g *gitlabProvider) PullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error) {
	var mr gitlabMR
	path := fmt.Sprintf("projects/%s/merge_requests/%d", project(repo), number)
	if err := g.c.do(ctx, http.MethodGet, path, nil, &mr); err != nil {
		return nil, err
	}
	return mr.convert(), nil
}
