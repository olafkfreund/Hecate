package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

// fakeHost stands in for a git host's API.
type fakeHost struct {
	pr        *provider.PullRequest
	opened    []provider.PullRequestSpec
	openErr   error
	readErr   error
	statuses  []provider.CommitStatus
	statusErr error
}

func (f *fakeHost) SetCommitStatus(_ context.Context, s provider.CommitStatus) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.statuses = append(f.statuses, s)
	return nil
}

func (f *fakeHost) Kind() provider.Kind { return provider.GitHub }

func (f *fakeHost) EnsurePullRequest(
	_ context.Context, spec provider.PullRequestSpec,
) (*provider.PullRequest, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened = append(f.opened, spec)
	if f.pr == nil {
		f.pr = &provider.PullRequest{Number: 7, URL: "https://github.test/pull/7", State: provider.Open}
	}
	f.pr.Head = spec.Head
	return f.pr, nil
}

func (f *fakeHost) PullRequest(_ context.Context, _ provider.Repo, _ int) (*provider.PullRequest, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.pr, nil
}

func prStep(t *testing.T, host *fakeHost, objects ...runtime.Object) *GitPullRequest {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objects {
		builder = builder.WithRuntimeObjects(o)
	}
	step := NewGitPullRequest(builder.Build())
	step.providers = func(provider.Kind, provider.Config) (provider.Provider, error) { return host, nil }
	return step
}

func tokenSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "forge", Namespace: "acme"},
		Data:       map[string][]byte{"token": []byte("s3cret")},
	}
}

// prCtx builds a context with a real checkout, because the step reads the
// repository and base branch out of one rather than making the author restate
// what git-clone already knows.
func prCtx(t *testing.T, cfg GitPullRequestConfig) *passage.StepContext {
	t.Helper()
	work := t.TempDir()
	origin := originRepo(t)
	if _, err := NewGitClone(nil).Run(context.Background(), gitCtx(t, work, GitCloneConfig{
		Repo: origin,
	})); err != nil {
		t.Fatal(err)
	}
	sc := gitCtx(t, work, cfg)
	sc.Bundle = &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo-abc123", Namespace: "acme"},
		Spec:       v1alpha1.BundleSpec{Beacon: "podinfo", Alias: "wandering-owl"},
	}
	return sc
}

func TestPullRequestInfersWhatItCan(t *testing.T) {
	host := &fakeHost{}
	sc := prCtx(t, GitPullRequestConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
		// The repository URL is a local path in this test, so it has to be given.
		Repo:         "https://github.com/acme/fleet.git",
		WaitForMerge: ptr(false),
	})

	res, err := prStep(t, host, tokenSecret()).Run(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("phase = %s: %s", res.Phase, res.Message)
	}

	opened := host.opened[0]
	if opened.Repo.Slug() != "acme/fleet" {
		t.Errorf("repo = %s", opened.Repo)
	}
	// git-push's toNewBranch convention, so the common flow needs no wiring.
	if opened.Head != "hecate/production-abc123" {
		t.Errorf("head = %q", opened.Head)
	}
	// The base is the branch git-clone checked out.
	if opened.Base != "master" && opened.Base != "main" {
		t.Errorf("base = %q, want the checked-out branch", opened.Base)
	}
	if !strings.Contains(opened.Title, "wandering-owl") || !strings.Contains(opened.Title, "production") {
		t.Errorf("title = %q — it should say what is being promoted where", opened.Title)
	}
	if res.Output["number"] != 7 {
		t.Errorf("output number = %v", res.Output["number"])
	}
}

// The default has to be waiting. A crossing that succeeded the moment the pull
// request opened would record the Bundle as having cleared production before
// anybody looked at it.
func TestPullRequestWaitsByDefault(t *testing.T) {
	host := &fakeHost{}
	sc := prCtx(t, GitPullRequestConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
		Repo:           "https://github.com/acme/fleet.git",
	})

	res, err := prStep(t, host, tokenSecret()).Run(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != v1alpha1.StepRunning {
		t.Fatalf("phase = %s, want Running: %s", res.Phase, res.Message)
	}
	if res.RetryAfter != defaultPollInterval {
		t.Errorf("retryAfter = %s", res.RetryAfter)
	}
	if !strings.Contains(res.Message, "waiting") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestPullRequestOutcomes(t *testing.T) {
	base := func() GitPullRequestConfig {
		return GitPullRequestConfig{
			CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
			Repo:           "https://github.com/acme/fleet.git",
			PollInterval:   &metav1.Duration{Duration: 5 * time.Minute},
		}
	}

	t.Run("a merge finishes the step and publishes the merge commit", func(t *testing.T) {
		// The merge commit, not the branch's: a squashing host lands the change
		// under a hash that never existed locally, and that is what a later
		// flux-wait must wait for.
		host := &fakeHost{pr: &provider.PullRequest{
			Number: 7, URL: "https://github.test/pull/7",
			State: provider.Merged, MergeCommit: "cafebabe0000",
		}}
		res, err := prStep(t, host, tokenSecret()).Run(context.Background(), prCtx(t, base()))
		if err != nil {
			t.Fatal(err)
		}
		if res.Phase != v1alpha1.StepSucceeded {
			t.Fatalf("phase = %s", res.Phase)
		}
		if res.Output["sha"] != "cafebabe0000" {
			t.Errorf("sha = %v", res.Output["sha"])
		}
	})

	t.Run("a closed pull request ends the crossing", func(t *testing.T) {
		host := &fakeHost{pr: &provider.PullRequest{
			Number: 7, URL: "https://github.test/pull/7", State: provider.Closed,
		}}
		_, err := prStep(t, host, tokenSecret()).Run(context.Background(), prCtx(t, base()))
		if !passage.IsTerminal(err) {
			t.Fatalf("err = %v, want terminal — waiting longer cannot reopen it", err)
		}
		if passage.ReasonOf(err) != ReasonPullRequestClosed {
			t.Errorf("reason = %s", passage.ReasonOf(err))
		}
	})

	t.Run("the poll interval is configurable", func(t *testing.T) {
		res, err := prStep(t, &fakeHost{}, tokenSecret()).Run(context.Background(), prCtx(t, base()))
		if err != nil {
			t.Fatal(err)
		}
		if res.RetryAfter != 5*time.Minute {
			t.Errorf("retryAfter = %s", res.RetryAfter)
		}
	})
}

func TestPullRequestFailures(t *testing.T) {
	cfg := GitPullRequestConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
		Repo:           "https://github.com/acme/fleet.git",
	}

	t.Run("a rejected token is terminal", func(t *testing.T) {
		host := &fakeHost{openErr: &provider.APIError{Status: 401, Message: "Bad credentials"}}
		_, err := prStep(t, host, tokenSecret()).Run(context.Background(), prCtx(t, cfg))
		if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonProviderAuthFailed {
			t.Errorf("err = %v, reason = %s", err, passage.ReasonOf(err))
		}
	})

	t.Run("a host outage is retryable", func(t *testing.T) {
		host := &fakeHost{openErr: &provider.APIError{Status: 503, Message: "unavailable"}}
		_, err := prStep(t, host, tokenSecret()).Run(context.Background(), prCtx(t, cfg))
		if err == nil || passage.IsTerminal(err) {
			t.Errorf("err = %v, want a retryable failure", err)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		_, err := prStep(t, &fakeHost{}).Run(context.Background(),
			prCtx(t, GitPullRequestConfig{Repo: "https://github.com/acme/fleet.git"}))
		if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonInvalidConfig {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a Secret with no token in it", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "forge", Namespace: "acme"},
			Data:       map[string][]byte{"username": []byte("hecate")},
		}
		_, err := prStep(t, &fakeHost{}, empty).Run(context.Background(), prCtx(t, cfg))
		if !passage.IsTerminal(err) || !strings.Contains(err.Error(), "no token") {
			t.Errorf("err = %v", err)
		}
	})
}

// A step whose config named a host nobody can identify must say what to do,
// not fail with a nil provider somewhere further in.
func TestPullRequestRefusesAnUnknownHost(t *testing.T) {
	step := NewGitPullRequest(prStep(t, &fakeHost{}, tokenSecret()).client)
	_, err := step.Run(context.Background(), prCtx(t, GitPullRequestConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
		Repo:           "https://git.acme.io/acme/fleet.git",
	}))
	if !passage.IsTerminal(err) || !strings.Contains(err.Error(), "provider: github") {
		t.Errorf("err = %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
