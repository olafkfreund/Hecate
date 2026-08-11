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

type fakeGitLab struct {
	t       *testing.T
	mrs     []gitlabMR
	creates int
	token   string
	rawPath string
	// hideNextList makes the next listing come back empty — losing a race, or
	// reading a replica that has not caught up.
	hideNextList bool
}

func (f *fakeGitLab) start(t *testing.T) *httptest.Server {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.token = r.Header.Get("PRIVATE-TOKEN")
	// EscapedPath, not Path: the project path is URL-encoded, and a client that
	// let net/url decode the slashes would address a project that does not exist.
	f.rawPath = r.URL.EscapedPath()
	path := strings.TrimPrefix(f.rawPath, "/")

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/merge_requests"):
		branch := r.URL.Query().Get("source_branch")
		var out []gitlabMR
		if f.hideNextList {
			f.hideNextList = false
			write(w, http.StatusOK, out)
			return
		}
		for _, mr := range f.mrs {
			if mr.State == "opened" && mr.SourceBranch == branch {
				out = append(out, mr)
			}
		}
		write(w, http.StatusOK, out)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/merge_requests"):
		f.creates++
		var body struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
			Labels       string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, mr := range f.mrs {
			if mr.State == "opened" && mr.SourceBranch == body.SourceBranch {
				write(w, http.StatusConflict, map[string]any{
					"message": []string{"Another open merge request already exists for this source branch"},
				})
				return
			}
		}
		if body.Labels != "promotion,production" {
			f.t.Errorf("labels = %q, want them set at creation", body.Labels)
		}
		mr := gitlabMR{
			IID: len(f.mrs) + 1, WebURL: "https://gitlab.test/-/merge_requests/1",
			State: "opened", SourceBranch: body.SourceBranch,
		}
		f.mrs = append(f.mrs, mr)
		write(w, http.StatusCreated, mr)

	case r.Method == http.MethodGet && strings.Contains(path, "/merge_requests/"):
		var n int
		_, _ = fmt.Sscanf(path[strings.LastIndex(path, "/")+1:], "%d", &n)
		for _, mr := range f.mrs {
			if mr.IID == n {
				write(w, http.StatusOK, mr)
				return
			}
		}
		write(w, http.StatusNotFound, map[string]string{"message": "404 Not found"})

	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		write(w, http.StatusNotFound, map[string]string{"message": "404 Not found"})
	}
}

func gitlabAt(t *testing.T, url string) Provider {
	t.Helper()
	p, err := New(GitLab, Config{BaseURL: url + "/api/v4", Token: "gl-token"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A nested group is the normal shape of a self-managed GitLab, and the project
// path has to reach the API encoded or it addresses nothing.
var delivery = Repo{Host: "gitlab.acme.io", Owner: "platform/delivery", Name: "fleet"}

func gitlabSpec() PullRequestSpec {
	return PullRequestSpec{
		Repo: delivery, Head: "hecate/production-abc123", Base: "main",
		Title: "Promote wandering-owl to production", Body: "Bundle podinfo-abc123",
		Labels: []string{"promotion", "production"},
	}
}

func TestGitLabEnsureIsIdempotent(t *testing.T) {
	fake := &fakeGitLab{}
	gl := gitlabAt(t, fake.start(t).URL)

	first, err := gl.EnsurePullRequest(context.Background(), gitlabSpec())
	if err != nil {
		t.Fatal(err)
	}
	second, err := gl.EnsurePullRequest(context.Background(), gitlabSpec())
	if err != nil {
		t.Fatal(err)
	}

	if first.Number != second.Number || fake.creates != 1 {
		t.Errorf("opened %d merge requests, numbered %d then %d", fake.creates, first.Number, second.Number)
	}
	if !strings.Contains(fake.rawPath, "platform%2Fdelivery%2Ffleet") {
		t.Errorf("the nested group was not encoded into the path: %s", fake.rawPath)
	}
	if fake.token != "gl-token" {
		t.Errorf("PRIVATE-TOKEN = %q", fake.token)
	}
}

func TestGitLabAdoptsAConflictingMergeRequest(t *testing.T) {
	fake := &fakeGitLab{}
	gl := gitlabAt(t, fake.start(t).URL)

	if _, err := gl.EnsurePullRequest(context.Background(), gitlabSpec()); err != nil {
		t.Fatal(err)
	}
	fake.creates = 0
	fake.hideNextList = true

	got, err := gl.EnsurePullRequest(context.Background(), gitlabSpec())
	if err != nil {
		t.Fatalf("a 409 should be adopted, not fatal: %v", err)
	}
	if fake.creates != 1 {
		t.Fatal("the create path was not exercised, so the conflict recovery was not tested")
	}
	if got.Number != 1 {
		t.Errorf("adopted %d, want the existing 1", got.Number)
	}
}

func TestGitLabMergeState(t *testing.T) {
	fake := &fakeGitLab{mrs: []gitlabMR{
		{IID: 1, State: "opened"},
		{IID: 2, State: "merged", MergeCommitSHA: "abc123"},
		// A squashed merge leaves merge_commit_sha empty and puts the commit
		// that actually landed in squash_commit_sha. Reporting nothing here
		// would leave a later flux-wait with no revision to wait for.
		{IID: 3, State: "merged", SquashCommitSHA: "5quash00"},
		{IID: 4, State: "closed"},
		// Locked is still open, just not editable.
		{IID: 5, State: "locked"},
	}}
	gl := gitlabAt(t, fake.start(t).URL)

	for _, tc := range []struct {
		number      int
		state       State
		mergeCommit string
	}{
		{1, Open, ""},
		{2, Merged, "abc123"},
		{3, Merged, "5quash00"},
		{4, Closed, ""},
		{5, Open, ""},
	} {
		pr, err := gl.PullRequest(context.Background(), delivery, tc.number)
		if err != nil {
			t.Fatal(err)
		}
		if pr.State != tc.state || pr.MergeCommit != tc.mergeCommit {
			t.Errorf("!%d: state = %s, mergeCommit = %q; want %s, %q",
				tc.number, pr.State, pr.MergeCommit, tc.state, tc.mergeCommit)
		}
	}
}

func TestGitLabErrors(t *testing.T) {
	fake := &fakeGitLab{}
	gl := gitlabAt(t, fake.start(t).URL)

	_, err := gl.PullRequest(context.Background(), delivery, 99)
	if !IsNotFound(err) {
		t.Errorf("a missing merge request should be recognisable: %v", err)
	}
	// GitLab reports validation failures as a list of messages, and a client
	// that only understood strings would hand the operator raw JSON.
	if !strings.Contains(err.Error(), "404 Not found") {
		t.Errorf("the host's own message was lost: %v", err)
	}
	if _, err := New(GitLab, Config{}); err == nil {
		t.Error("expected a refusal without a token")
	}
}
