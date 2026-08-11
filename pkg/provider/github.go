package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// githubAPI is the public host's API root. GitHub Enterprise Server serves the
// same API under /api/v3 on the appliance's own hostname, which is why the base
// URL is configurable from the start rather than retrofitted.
const githubAPI = "https://api.github.com"

type githubProvider struct{ c *client }

func newGitHub(cfg Config) (Provider, error) {
	if cfg.Token == "" {
		return nil, errors.New("github: no token")
	}
	c, err := newClient(cfg.BaseURL, githubAPI, map[string]string{
		"Authorization": "Bearer " + cfg.Token,
		"Accept":        "application/vnd.github+json",
		// Pinned, so a future default version cannot change what our parsing
		// means without us choosing it.
		"X-GitHub-Api-Version": "2022-11-28",
		"User-Agent":           "hecate",
	})
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	return &githubProvider{c: c}, nil
}

func (g *githubProvider) Kind() Kind { return GitHub }

// githubPR is the subset of the pull request payload a promotion cares about.
type githubPR struct {
	Number        int    `json:"number"`
	HTMLURL       string `json:"html_url"`
	State         string `json:"state"`
	Merged        bool   `json:"merged"`
	MergeCommitSA string `json:"merge_commit_sha"`
	Head          struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (p githubPR) convert() *PullRequest {
	pr := &PullRequest{
		Number: p.Number, URL: p.HTMLURL, Head: p.Head.Ref, MergeCommit: p.MergeCommitSA,
	}
	switch {
	case p.Merged:
		pr.State = Merged
	case p.State == "open":
		pr.State = Open
	default:
		pr.State = Closed
	}
	// A closed pull request carries merge_commit_sha whether or not it merged,
	// so reporting it on one that was closed unmerged would hand a later step a
	// revision to wait for that will never appear on the base branch.
	if pr.State != Merged {
		pr.MergeCommit = ""
	}
	return pr
}

func (g *githubProvider) EnsurePullRequest(ctx context.Context, spec PullRequestSpec) (*PullRequest, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	if existing, err := g.openFor(ctx, spec.Repo, spec.Head); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	var created githubPR
	err := g.c.do(ctx, http.MethodPost, "repos/"+spec.Repo.Slug()+"/pulls", map[string]any{
		"title": spec.Title, "head": spec.Head, "base": spec.Base, "body": spec.Body,
	}, &created)
	if err != nil {
		// Two Passages racing, or our own retry arriving after the create took
		// effect but before we saw the answer. Either way the pull request the
		// step wants now exists, so find it rather than fail.
		if statusIs(err, http.StatusUnprocessableEntity) && strings.Contains(err.Error(), "already exists") {
			if existing, lookupErr := g.openFor(ctx, spec.Repo, spec.Head); lookupErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}

	pr := created.convert()
	if len(spec.Labels) > 0 {
		// Labels are routing and reporting, not correctness: a pull request
		// that opened but could not be labelled is still the promotion the Gate
		// asked for, and failing here would strand it.
		_ = g.c.do(ctx, http.MethodPost,
			fmt.Sprintf("repos/%s/issues/%d/labels", spec.Repo.Slug(), pr.Number),
			map[string]any{"labels": spec.Labels}, nil)
	}
	return pr, nil
}

// openFor finds the open pull request for a head branch, or nil.
func (g *githubProvider) openFor(ctx context.Context, repo Repo, head string) (*PullRequest, error) {
	// GitHub wants the head qualified by the owner, and answers an unqualified
	// branch name with every repository's branch of that name.
	query := fmt.Sprintf("repos/%s/pulls?state=open&head=%s:%s",
		repo.Slug(), ownerOf(repo), head)

	var found []githubPR
	if err := g.c.do(ctx, http.MethodGet, query, nil, &found); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return found[0].convert(), nil
}

// ownerOf is the org or user the head branch lives under. Hecate pushes to the
// same repository rather than to a fork, so that is the repository's owner.
func ownerOf(repo Repo) string {
	if i := strings.LastIndex(repo.Owner, "/"); i >= 0 {
		return repo.Owner[i+1:]
	}
	return repo.Owner
}

func (g *githubProvider) PullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error) {
	var pr githubPR
	err := g.c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", repo.Slug(), number), nil, &pr)
	if err != nil {
		return nil, err
	}
	return pr.convert(), nil
}

func (s PullRequestSpec) validate() error {
	switch {
	case s.Repo.Owner == "" || s.Repo.Name == "":
		return errors.New("no repository")
	case s.Head == "":
		return errors.New("no head branch")
	case s.Base == "":
		return errors.New("no base branch")
	case s.Title == "":
		return errors.New("no title")
	}
	return nil
}
