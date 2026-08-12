package steps

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/provider"
)

func statusStep(t *testing.T, host *fakeHost) *CommitStatus {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	step := NewCommitStatus(fake.NewClientBuilder().
		WithScheme(scheme).WithRuntimeObjects(tokenSecret()).Build())
	step.providers = func(provider.Kind, provider.Config) (provider.Provider, error) { return host, nil }
	return step
}

func statusCtx(t *testing.T, failed bool, cfg CommitStatusConfig) *passage.StepContext {
	t.Helper()
	if cfg.CredentialsRef == nil {
		cfg.CredentialsRef = &v1alpha1.LocalSecretRef{Name: "forge"}
	}
	sc := prCtx(t, GitPullRequestConfig{})
	raw := gitCtx(t, sc.WorkDir, cfg)
	raw.Failed = failed
	raw.Bundle = sc.Bundle
	return raw
}

// The whole point of the step: run with `if: always` and it reports what
// actually happened. A step that could only ever be green would look like
// coverage and be none.
func TestCommitStatusReportsTheRealOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
		want   provider.CommitState
	}{
		{"a crossing that worked", false, provider.StateSuccess},
		{"a crossing that did not", true, provider.StateFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeHost{}
			sc := statusCtx(t, tc.failed, CommitStatusConfig{
				Repo: "https://github.com/acme/fleet.git",
			})

			res, err := statusStep(t, host).Run(context.Background(), sc)
			if err != nil {
				t.Fatal(err)
			}
			if res.Phase != v1alpha1.StepSucceeded {
				t.Fatalf("phase = %s: %s", res.Phase, res.Message)
			}
			if len(host.statuses) != 1 {
				t.Fatalf("reported %d statuses, want 1", len(host.statuses))
			}
			got := host.statuses[0]
			if got.State != tc.want {
				t.Errorf("state = %s, want %s — the engine already knows, the step "+
					"must not need telling", got.State, tc.want)
			}
			// One context per Gate, so two Gates reporting on the same commit
			// do not overwrite one another.
			if got.Context != "hecate/production" {
				t.Errorf("context = %q", got.Context)
			}
			if got.SHA == "" || len(got.SHA) != 40 {
				t.Errorf("sha = %q, want the checkout's HEAD", got.SHA)
			}
			if tc.failed && !strings.Contains(got.Description, "did not cross") {
				t.Errorf("description = %q, want it to say the crossing failed", got.Description)
			}
		})
	}
}

// Pending at the top of a Passage is the one case for saying the state
// explicitly, and it must win over what the engine reports.
func TestCommitStatusHonoursAnExplicitState(t *testing.T) {
	host := &fakeHost{}
	sc := statusCtx(t, false, CommitStatusConfig{
		Repo: "https://github.com/acme/fleet.git", State: "Pending",
	})

	if _, err := statusStep(t, host).Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if got := host.statuses[0].State; got != provider.StatePending {
		t.Errorf("state = %s, want Pending", got)
	}
}

// An unknown state is a Gate that will never work. Refusing at admission beats
// discovering it after the commit is already pushed.
func TestCommitStatusRefusesAnUnknownStateAtAdmission(t *testing.T) {
	err := NewCommitStatus(nil).Check([]byte(`{"state":"Greenish"}`))
	if err == nil || !strings.Contains(err.Error(), "Greenish") {
		t.Fatalf("err = %v, want one naming the bad state", err)
	}
	if !strings.Contains(err.Error(), "Pending") {
		t.Errorf("err = %v, want it to say what is allowed", err)
	}
}

// Reporting needs an API token, which is not what push access is.
func TestCommitStatusRequiresCredentials(t *testing.T) {
	sc := statusCtx(t, false, CommitStatusConfig{Repo: "https://github.com/acme/fleet.git"})
	sc.Config = []byte(`{"repo":"https://github.com/acme/fleet.git"}`)

	_, err := statusStep(t, &fakeHost{}).Run(context.Background(), sc)
	if !passage.IsTerminal(err) {
		t.Fatalf("err = %v, want terminal — a missing credentialsRef will not appear", err)
	}
	if !strings.Contains(err.Error(), "credentialsRef") {
		t.Errorf("err = %v, want it to name the missing field", err)
	}
}

func TestCommitStatusClassifiesHostRefusals(t *testing.T) {
	host := &fakeHost{statusErr: &provider.APIError{Status: 401, Message: "Bad credentials"}}
	sc := statusCtx(t, true, CommitStatusConfig{Repo: "https://github.com/acme/fleet.git"})

	_, err := statusStep(t, host).Run(context.Background(), sc)
	if !passage.IsTerminal(err) || passage.ReasonOf(err) != ReasonProviderAuthFailed {
		t.Errorf("err = %v, reason = %s, want a terminal auth failure", err, passage.ReasonOf(err))
	}
}

// The work dir is disposable (D19). A step reporting an outcome after a restart
// has no checkout to read, and the message has to say what to do about it
// rather than surface a bare git error.
func TestCommitStatusExplainsAMissingCheckout(t *testing.T) {
	host := &fakeHost{}
	sc := gitCtx(t, t.TempDir(), CommitStatusConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
	})

	_, err := statusStep(t, host).Run(context.Background(), sc)
	if err == nil {
		t.Fatal("want an error with no checkout and no explicit sha")
	}
	if !strings.Contains(err.Error(), "sha") || !strings.Contains(err.Error(), "repo") {
		t.Errorf("err = %v, want it to name the two fields that would fix it", err)
	}
}

// Given both, it needs no checkout at all — which is what lets the step run
// after a restart, or in a Passage that never cloned anything.
func TestCommitStatusNeedsNoCheckoutWhenToldEverything(t *testing.T) {
	host := &fakeHost{}
	sc := gitCtx(t, t.TempDir(), CommitStatusConfig{
		CredentialsRef: &v1alpha1.LocalSecretRef{Name: "forge"},
		Repo:           "https://github.com/acme/fleet.git",
		SHA:            "cafebabecafebabecafebabecafebabecafebabe",
	})

	if _, err := statusStep(t, host).Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if host.statuses[0].SHA != "cafebabecafebabecafebabecafebabecafebabe" {
		t.Errorf("sha = %q", host.statuses[0].SHA)
	}
}
