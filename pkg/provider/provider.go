// Package provider talks to the git hosts' APIs — the part of a promotion that
// plain git cannot do.
//
// The boundary is deliberate and narrow. Cloning, committing and pushing work
// against any host over HTTPS or SSH with no provider code at all, and Hecate
// does them that way. What needs an API is the review: opening a pull request,
// learning whether it merged, and reporting a commit status. That is all this
// package covers, which is why adding a host is small.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Kind identifies a host's API flavour.
type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// Repo identifies a repository on a host.
type Repo struct {
	// Host is the API host, e.g. github.com or gitlab.example.com.
	Host string
	// Owner is the user, organisation or group path. GitLab groups nest, so
	// this can contain slashes.
	Owner string
	// Name is the repository itself.
	Name string
}

// Slug is owner/name, the form both APIs use in paths.
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

func (r Repo) String() string { return r.Host + "/" + r.Slug() }

// ParseRepo pulls a repository out of a git clone URL, in either of the two
// forms people actually write:
//
//	https://github.com/acme/fleet.git
//	git@gitlab.example.com:group/sub/fleet.git
func ParseRepo(cloneURL string) (Repo, error) {
	raw := strings.TrimSpace(cloneURL)
	if raw == "" {
		return Repo{}, errors.New("no repository URL")
	}

	var host, path string
	if scp, ok := scpSyntax(raw); ok {
		host, path = scp[0], scp[1]
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return Repo{}, fmt.Errorf("%q is not a repository URL: %w", cloneURL, err)
		}
		if u.Host == "" {
			return Repo{}, fmt.Errorf("%q has no host", cloneURL)
		}
		host, path = u.Hostname(), u.Path
	}

	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	owner, name, found := cut(path)
	if !found {
		return Repo{}, fmt.Errorf("%q names no repository under a host", cloneURL)
	}
	return Repo{Host: host, Owner: owner, Name: name}, nil
}

// scpSyntax matches `git@host:path`, which is not a URL and which url.Parse
// silently mangles into a scheme-less path.
func scpSyntax(raw string) ([2]string, bool) {
	if strings.Contains(raw, "://") {
		return [2]string{}, false
	}
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	if colon < 0 || colon < at {
		return [2]string{}, false
	}
	return [2]string{raw[at+1 : colon], raw[colon+1:]}, true
}

// cut splits a path into everything-but-the-last segment and the last one, so
// a nested GitLab group stays with the owner.
func cut(path string) (owner, name string, ok bool) {
	i := strings.LastIndex(path, "/")
	if i <= 0 || i == len(path)-1 {
		return "", "", false
	}
	return path[:i], path[i+1:], true
}

// KindFor guesses the API flavour from a host. It only knows the two public
// hosts by name: a self-managed GitLab or a GitHub Enterprise Server is
// indistinguishable by hostname, so those must say which they are.
func KindFor(host string) Kind {
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return GitHub
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return GitLab
	default:
		return ""
	}
}

// State is where a pull request has got to.
type State string

const (
	Open   State = "Open"
	Merged State = "Merged"
	Closed State = "Closed"
)

// PullRequest is a change awaiting review. GitLab calls it a merge request;
// the shape is the same and the vocabulary difference stops at the API client.
type PullRequest struct {
	Number int
	URL    string
	State  State
	// MergeCommit is the commit the merge produced, once it has merged. It is
	// what a later flux-wait waits for — the branch commit never lands on the
	// base branch under its own hash when the host squashes.
	MergeCommit string
	// Head is the branch the change is on.
	Head string
}

// PullRequestSpec is what to open.
type PullRequestSpec struct {
	Repo   Repo
	Head   string
	Base   string
	Title  string
	Body   string
	Labels []string
}

// Provider is a git host's API, as far as a promotion needs it.
type Provider interface {
	// Kind is which host flavour this is.
	Kind() Kind
	// EnsurePullRequest opens a pull request, or returns the one already open
	// for the same head branch.
	//
	// Not "Create": a step is re-entrant (D19), so it will call this again
	// after a requeue, and a second identical pull request is worse than none.
	EnsurePullRequest(ctx context.Context, spec PullRequestSpec) (*PullRequest, error)
	// PullRequest reads one back.
	PullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error)
}

// Config is what a provider needs to reach a host.
type Config struct {
	// BaseURL is the API root. Empty means the public host's. Set it for GitHub
	// Enterprise Server and self-managed GitLab — the organisations that care
	// most about promotion gates are mostly not on the public hosts.
	BaseURL string
	// Token authenticates. A personal, project or installation access token.
	Token string
}

// New builds a provider.
//
// A switch rather than a registry: there are two hosts, and a registration
// mechanism for two implementations is a mechanism to maintain for nothing.
func New(kind Kind, cfg Config) (Provider, error) {
	switch kind {
	case GitHub:
		return newGitHub(cfg)
	case GitLab:
		return nil, fmt.Errorf("the gitlab provider is not implemented yet")
	case "":
		return nil, errors.New("no provider given, and the host is not one whose flavour can be guessed — " +
			"set provider: github or provider: gitlab")
	default:
		return nil, fmt.Errorf("no provider named %q", kind)
	}
}
