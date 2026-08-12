package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recorded is one status the host was asked to set.
type recorded struct {
	path string
	body map[string]any
}

// statusHost accepts commit statuses and remembers them, so the assertions can
// be about what the host was actually told rather than about our own structs.
type statusHost struct {
	got []recorded
	// reject is returned instead of accepting, to exercise the error paths.
	reject func(w http.ResponseWriter)
}

func (h *statusHost) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// EscapedPath, not Path: the server decodes Path, so a GitLab project
		// ID escaped as platform%2Fteam%2Ffleet would read back as a directory
		// path and the assertion would be testing nothing.
		h.got = append(h.got, recorded{path: r.URL.EscapedPath(), body: body})
		if h.reject != nil {
			h.reject(w)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubCommitStatus(t *testing.T) {
	host := &statusHost{}
	srv := host.start(t)
	p, err := New(GitHub, Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	err = p.SetCommitStatus(context.Background(), CommitStatus{
		Repo:        Repo{Host: "github.com", Owner: "acme", Name: "fleet"},
		SHA:         "cafebabe",
		State:       StateFailure,
		Context:     "hecate/production",
		Description: "podinfo-abc did not cross production",
		TargetURL:   "https://hecate.example/p/1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(host.got) != 1 {
		t.Fatalf("host was called %d times, want 1", len(host.got))
	}
	call := host.got[0]
	if call.path != "/repos/acme/fleet/statuses/cafebabe" {
		t.Errorf("path = %q", call.path)
	}
	// "failure", not our "Failure": the host's vocabulary, not ours.
	if call.body["state"] != "failure" {
		t.Errorf("state = %v, want GitHub's spelling", call.body["state"])
	}
	if call.body["context"] != "hecate/production" {
		t.Errorf("context = %v", call.body["context"])
	}
	if call.body["target_url"] != "https://hecate.example/p/1" {
		t.Errorf("target_url = %v", call.body["target_url"])
	}
}

func TestGitLabCommitStatus(t *testing.T) {
	host := &statusHost{}
	srv := host.start(t)
	p, err := New(GitLab, Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	// A nested group, because GitLab paths are escaped project IDs rather than
	// owner/name and that is the case which breaks a naive join.
	err = p.SetCommitStatus(context.Background(), CommitStatus{
		Repo:    Repo{Host: "gitlab.example", Owner: "platform/team", Name: "fleet"},
		SHA:     "cafebabe",
		State:   StateSuccess,
		Context: "hecate/staging",
	})
	if err != nil {
		t.Fatal(err)
	}

	call := host.got[0]
	if call.path != "/projects/platform%2Fteam%2Ffleet/statuses/cafebabe" {
		t.Errorf("path = %q, want the project path escaped", call.path)
	}
	// GitLab calls the check `name`, and spells failure `failed`.
	if call.body["name"] != "hecate/staging" {
		t.Errorf("name = %v", call.body["name"])
	}
	if call.body["state"] != "success" {
		t.Errorf("state = %v", call.body["state"])
	}
}

func TestGitLabStatesUseGitLabsSpelling(t *testing.T) {
	for state, want := range map[CommitState]string{
		StatePending: "pending",
		StateSuccess: "success",
		StateFailure: "failed", // not "failure" — GitLab rejects that
	} {
		host := &statusHost{}
		srv := host.start(t)
		p, _ := New(GitLab, Config{BaseURL: srv.URL, Token: "t"})
		if err := p.SetCommitStatus(context.Background(), CommitStatus{
			Repo: Repo{Host: "gitlab.com", Owner: "acme", Name: "fleet"},
			SHA:  "abc", State: state, Context: "hecate/x",
		}); err != nil {
			t.Fatal(err)
		}
		if got := host.got[0].body["state"]; got != want {
			t.Errorf("%s -> %v, want %q", state, got, want)
		}
	}
}

// GitLab models a commit status as a state machine and refuses a transition to
// the state it is already in. Steps are re-entrant (D19), so a retried crossing
// reports the same state again as a matter of course — failing the Passage for
// successfully having done what it was asked would be absurd.
func TestGitLabToleratesReportingTheSameStateTwice(t *testing.T) {
	host := &statusHost{reject: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Cannot transition status via :run from :running"}`))
	}}
	srv := host.start(t)
	p, _ := New(GitLab, Config{BaseURL: srv.URL, Token: "t"})

	if err := p.SetCommitStatus(context.Background(), CommitStatus{
		Repo: Repo{Host: "gitlab.com", Owner: "acme", Name: "fleet"},
		SHA:  "abc", State: StateSuccess, Context: "hecate/x",
	}); err != nil {
		t.Errorf("a repeated status must succeed, got %v", err)
	}
}

// Any other 400 is a real problem and must not be swallowed by the tolerance
// above.
func TestGitLabStillReportsOtherRefusals(t *testing.T) {
	host := &statusHost{reject: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Commit not found"}`))
	}}
	srv := host.start(t)
	p, _ := New(GitLab, Config{BaseURL: srv.URL, Token: "t"})

	err := p.SetCommitStatus(context.Background(), CommitStatus{
		Repo: Repo{Host: "gitlab.com", Owner: "acme", Name: "fleet"},
		SHA:  "abc", State: StateSuccess, Context: "hecate/x",
	})
	if err == nil || !strings.Contains(err.Error(), "Commit not found") {
		t.Errorf("err = %v, want the host's refusal reported", err)
	}
}

// Refused before a request is made, so the mistake is reported against the
// Gate rather than as somebody else's 422.
func TestCommitStatusValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status CommitStatus
		want   string
	}{
		{"no commit", CommitStatus{State: StateSuccess, Context: "x"}, "no commit"},
		{"no context", CommitStatus{SHA: "abc", State: StateSuccess}, "no context"},
		{"unknown state", CommitStatus{SHA: "abc", Context: "x", State: "Broken"}, "unknown commit state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &statusHost{}
			srv := host.start(t)
			p, _ := New(GitHub, Config{BaseURL: srv.URL, Token: "t"})

			err := p.SetCommitStatus(context.Background(), tc.status)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			if len(host.got) != 0 {
				t.Error("an invalid status was sent to the host anyway")
			}
		})
	}
}
