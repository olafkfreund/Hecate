package provider

import (
	"strings"
	"testing"
)

func TestParseRepo(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		want      Repo
		wantErr   bool
	}{
		{"https", "https://github.com/acme/fleet.git", Repo{"github.com", "acme", "fleet"}, false},
		{"https without .git", "https://github.com/acme/fleet", Repo{"github.com", "acme", "fleet"}, false},
		{"https with a port", "https://ghes.acme.io:8443/acme/fleet.git", Repo{"ghes.acme.io", "acme", "fleet"}, false},
		{"ssh scp form", "git@github.com:acme/fleet.git", Repo{"github.com", "acme", "fleet"}, false},
		{"ssh url form", "ssh://git@github.com/acme/fleet.git", Repo{"github.com", "acme", "fleet"}, false},
		// GitLab groups nest, and flattening one loses the path the API needs.
		{"a nested group", "https://gitlab.acme.io/platform/delivery/fleet.git",
			Repo{"gitlab.acme.io", "platform/delivery", "fleet"}, false},
		{"a nested group over ssh", "git@gitlab.acme.io:platform/delivery/fleet.git",
			Repo{"gitlab.acme.io", "platform/delivery", "fleet"}, false},
		{"a local path", "/srv/git/fleet.git", Repo{}, true},
		{"no repository", "https://github.com/acme", Repo{}, true},
		{"nothing", "", Repo{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRepo(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestKindForOnlyGuessesThePublicHosts(t *testing.T) {
	for host, want := range map[string]Kind{
		"github.com":       GitHub,
		"gitlab.com":       GitLab,
		"ghes.acme.io":     "",
		"gitlab.acme.io":   "",
		"git.example.com":  "",
		"github.acme.io":   "", // an appliance, not the public host
		"api.github.com":   GitHub,
		"salsa.debian.org": "",
	} {
		if got := KindFor(host); got != want {
			t.Errorf("%s: got %q, want %q", host, got, want)
		}
	}
}

// A self-managed host cannot be identified by name, and guessing wrong sends
// GitLab requests to a GitHub API. The refusal has to say what to do about it.
func TestNewRefusesToGuess(t *testing.T) {
	_, err := New(KindFor("gitlab.acme.io"), Config{Token: "x"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "provider: github") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}
