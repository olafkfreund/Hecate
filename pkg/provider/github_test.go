package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub is enough of the API to exercise the four calls a promotion makes.
// It keeps state, because the re-entrancy this provider promises is only
// testable against something that remembers what it was already told.
type fakeGitHub struct {
	t       *testing.T
	prs     []githubPR
	labels  map[int][]string
	creates int
	auth    string
	path    string
	// hideNextList makes the next listing come back empty, which is what losing
	// a race — or reading a replica that has not caught up — looks like.
	hideNextList bool
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	f.t = t
	f.labels = map[int][]string{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.auth = r.Header.Get("Authorization")
	f.path = r.URL.Path
	path := strings.TrimPrefix(r.URL.Path, "/")

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls"):
		head := r.URL.Query().Get("head")
		var out []githubPR
		if f.hideNextList {
			f.hideNextList = false
			write(w, http.StatusOK, out)
			return
		}
		for _, pr := range f.prs {
			if pr.State == "open" && strings.HasSuffix(head, ":"+pr.Head.Ref) {
				out = append(out, pr)
			}
		}
		write(w, http.StatusOK, out)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
		f.creates++
		var body struct{ Title, Head, Base string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, pr := range f.prs {
			if pr.State == "open" && pr.Head.Ref == body.Head {
				write(w, http.StatusUnprocessableEntity, map[string]any{
					"message": "Validation Failed",
					"errors": []map[string]string{{
						"message": fmt.Sprintf("A pull request already exists for acme:%s.", body.Head),
					}},
				})
				return
			}
		}
		pr := githubPR{Number: len(f.prs) + 1, HTMLURL: "https://github.test/pull/1", State: "open"}
		pr.Head.Ref = body.Head
		f.prs = append(f.prs, pr)
		write(w, http.StatusCreated, pr)

	case r.Method == http.MethodPost && strings.Contains(path, "/issues/"):
		var body struct{ Labels []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		var n int
		_, _ = fmt.Sscanf(path[strings.Index(path, "/issues/")+8:], "%d", &n)
		f.labels[n] = body.Labels
		write(w, http.StatusOK, []any{})

	case r.Method == http.MethodGet && strings.Contains(path, "/pulls/"):
		var n int
		_, _ = fmt.Sscanf(path[strings.LastIndex(path, "/")+1:], "%d", &n)
		for _, pr := range f.prs {
			if pr.Number == n {
				write(w, http.StatusOK, pr)
				return
			}
		}
		write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})

	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func githubAt(t *testing.T, url string) Provider {
	t.Helper()
	p, err := New(GitHub, Config{BaseURL: url, Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var fleet = Repo{Host: "github.com", Owner: "acme", Name: "fleet"}

func spec() PullRequestSpec {
	return PullRequestSpec{
		Repo: fleet, Head: "hecate/production-abc123", Base: "main",
		Title: "promote podinfo 6.5.0", Body: "Bundle podinfo-abc123",
		Labels: []string{"promotion"},
	}
}

// A step re-runs after a requeue. Opening a second pull request for the same
// branch each time would bury the reviewer.
func TestEnsurePullRequestIsIdempotent(t *testing.T) {
	fake := &fakeGitHub{}
	srv := fake.start(t)
	gh := githubAt(t, srv.URL)

	first, err := gh.EnsurePullRequest(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	second, err := gh.EnsurePullRequest(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}

	if first.Number != second.Number {
		t.Errorf("opened a second pull request: %d then %d", first.Number, second.Number)
	}
	if fake.creates != 1 {
		t.Errorf("posted %d creates, want 1", fake.creates)
	}
	if first.State != Open {
		t.Errorf("state = %s", first.State)
	}
	if got := fake.labels[first.Number]; len(got) != 1 || got[0] != "promotion" {
		t.Errorf("labels = %v", got)
	}
}

// The lookup can lose a race with another Passage, or with our own earlier
// attempt. The host says so, and adopting the existing pull request is the only
// answer that lets the crossing continue.
func TestEnsurePullRequestAdoptsOneCreatedUnderneathUs(t *testing.T) {
	fake := &fakeGitHub{}
	srv := fake.start(t)
	gh := githubAt(t, srv.URL)

	if _, err := gh.EnsurePullRequest(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	fake.creates = 0
	fake.hideNextList = true

	got, err := gh.EnsurePullRequest(context.Background(), spec())
	if err != nil {
		t.Fatalf("a racing create should be adopted, not fatal: %v", err)
	}
	if fake.creates != 1 {
		t.Fatal("the create path was not exercised, so the 422 recovery was not tested")
	}
	if got.Number != 1 {
		t.Errorf("adopted %d, want the existing 1", got.Number)
	}
}

func TestPullRequestReportsMergeState(t *testing.T) {
	fake := &fakeGitHub{prs: []githubPR{
		{Number: 1, State: "open"},
		{Number: 2, State: "closed", Merged: true, MergeCommitSA: "abc123"},
		{Number: 3, State: "closed", MergeCommitSA: "def456"},
	}}
	srv := fake.start(t)
	gh := githubAt(t, srv.URL)

	for _, tc := range []struct {
		number      int
		state       State
		mergeCommit string
	}{
		{1, Open, ""},
		{2, Merged, "abc123"},
		// Closed unmerged still carries a merge_commit_sha. Reporting it would
		// hand a later flux-wait a revision that never reaches the base branch.
		{3, Closed, ""},
	} {
		pr, err := gh.PullRequest(context.Background(), fleet, tc.number)
		if err != nil {
			t.Fatal(err)
		}
		if pr.State != tc.state || pr.MergeCommit != tc.mergeCommit {
			t.Errorf("#%d: state = %s, mergeCommit = %q; want %s, %q",
				tc.number, pr.State, pr.MergeCommit, tc.state, tc.mergeCommit)
		}
	}
}

func TestGitHubErrors(t *testing.T) {
	fake := &fakeGitHub{}
	srv := fake.start(t)
	gh := githubAt(t, srv.URL)

	_, err := gh.PullRequest(context.Background(), fleet, 99)
	if !IsNotFound(err) {
		t.Errorf("a missing pull request should be recognisable: %v", err)
	}
	if IsAuth(err) {
		t.Error("a 404 is not an auth failure")
	}
	if fake.auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", fake.auth)
	}
}

func TestEnsurePullRequestValidates(t *testing.T) {
	fake := &fakeGitHub{}
	gh := githubAt(t, fake.start(t).URL)

	for name, s := range map[string]PullRequestSpec{
		"no base":  {Repo: fleet, Head: "x", Title: "t"},
		"no head":  {Repo: fleet, Base: "main", Title: "t"},
		"no title": {Repo: fleet, Head: "x", Base: "main"},
		"no repo":  {Head: "x", Base: "main", Title: "t"},
	} {
		if _, err := gh.EnsurePullRequest(context.Background(), s); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

// A GitHub Enterprise Server serves the same API under a path on its own host.
func TestGitHubEnterpriseBaseURL(t *testing.T) {
	fake := &fakeGitHub{}
	srv := fake.start(t)
	gh, err := New(GitHub, Config{BaseURL: srv.URL + "/api/v3", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.EnsurePullRequest(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fake.path, "/api/v3/") {
		t.Errorf("the appliance path prefix was dropped: %s", fake.path)
	}
}

func TestGitHubNeedsAToken(t *testing.T) {
	if _, err := New(GitHub, Config{}); err == nil {
		t.Error("expected a refusal")
	}
}
