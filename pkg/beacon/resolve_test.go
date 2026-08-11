package beacon

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// testRepo is a repository in an in-memory registry that tags can be pushed to
// over time, so a test can model a new release appearing rather than only a
// fixed starting state. Real registry behaviour, no network.
type testRepo struct {
	t       *testing.T
	Repo    string
	Digests map[string]string
	n       int
}

func newTestRepo(t *testing.T, repoName string, tags ...string) *testRepo {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, Repo: u.Host + "/" + repoName, Digests: map[string]string{}}
	r.Push(tags...)
	return r
}

// Push adds tags, each backed by a distinct image so digests differ and a wrong
// tag selection produces a wrong digest.
func (r *testRepo) Push(tags ...string) {
	r.t.Helper()
	for _, tag := range tags {
		r.n++
		img, err := random.Image(int64(64+r.n), 1)
		if err != nil {
			r.t.Fatal(err)
		}
		ref, err := name.NewTag(r.Repo + ":" + tag)
		if err != nil {
			r.t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			r.t.Fatal(err)
		}
		d, err := img.Digest()
		if err != nil {
			r.t.Fatal(err)
		}
		r.Digests[tag] = d.String()
	}
}

// pushTags is the common case: a repository with a fixed set of tags.
func pushTags(t *testing.T, repoName string, tags ...string) (repo string, digests map[string]string) {
	t.Helper()
	r := newTestRepo(t, repoName, tags...)
	return r.Repo, r.Digests
}

func TestResolveImage(t *testing.T) {
	repo, digests := pushTags(t, "acme/podinfo", "6.0.0", "6.1.0", "6.2.0", "7.0.0-beta.1", "latest")
	r := &Resolver{}

	t.Run("picks the highest release and pins its digest", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{
			Image: &v1alpha1.ImageWatch{Repo: repo},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Image == nil {
			t.Fatal("expected an image artifact")
		}
		if got.Image.Tag != "6.2.0" {
			t.Errorf("tag = %q, want 6.2.0", got.Image.Tag)
		}
		// The digest must be the one belonging to that tag, not merely present.
		if got.Image.Digest != digests["6.2.0"] {
			t.Errorf("digest = %q, want %q (the digest of 6.2.0)", got.Image.Digest, digests["6.2.0"])
		}
	})

	t.Run("constraint narrows the selection", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{
			Image: &v1alpha1.ImageWatch{Repo: repo, Constraint: "~6.1.0"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Image.Tag != "6.1.0" || got.Image.Digest != digests["6.1.0"] {
			t.Errorf("got %s@%s, want 6.1.0@%s", got.Image.Tag, got.Image.Digest, digests["6.1.0"])
		}
	})

	t.Run("resolution is stable across calls", func(t *testing.T) {
		// Idempotency starts here: an unchanged registry must resolve to an
		// identical artifact, or the controller mints a Bundle every poll.
		w := v1alpha1.WatchSource{Image: &v1alpha1.ImageWatch{Repo: repo}}
		first, err := r.Resolve(context.Background(), "acme", w)
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.Resolve(context.Background(), "acme", w)
		if err != nil {
			t.Fatal(err)
		}
		if *first.Image != *second.Image {
			t.Errorf("resolution not stable:\n  %+v\n  %+v", *first.Image, *second.Image)
		}
	})

	t.Run("no matching tag is ErrNoMatch, not a failure", func(t *testing.T) {
		_, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{
			Image: &v1alpha1.ImageWatch{Repo: repo, Constraint: "^99.0.0"},
		})
		var noMatch *ErrNoMatch
		if !errors.As(err, &noMatch) {
			t.Errorf("want ErrNoMatch, got %T: %v", err, err)
		}
	})
}

func TestResolveUnsupportedKinds(t *testing.T) {
	r := &Resolver{}
	for name, w := range map[string]v1alpha1.WatchSource{
		"chart": {Chart: &v1alpha1.ChartWatch{Repo: "https://charts.example.com", Name: "podinfo"}},
		"git":   {Git: &v1alpha1.GitWatch{Repo: "https://github.com/acme/app", Branch: "main"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), "acme", w)
			var unsupported *ErrUnsupported
			if !errors.As(err, &unsupported) {
				t.Errorf("want ErrUnsupported, got %T: %v", err, err)
			}
		})
	}

	t.Run("empty watch source", func(t *testing.T) {
		if _, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{}); err == nil {
			t.Error("expected an error for a watch source with nothing set")
		}
	})
}

// A declared field we cannot honour must say so, not pin the index digest while
// the user believes they pinned one platform.
func TestPlatformIsRefusedNotIgnored(t *testing.T) {
	_, err := (&Resolver{}).Resolve(context.Background(), "acme", v1alpha1.WatchSource{
		Image: &v1alpha1.ImageWatch{Repo: "ghcr.io/acme/app", Platform: "linux/arm64"},
	})
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("want ErrUnsupported, got %T: %v", err, err)
	}
}

func TestKeychainMissingSecretIsReported(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	_, err := r.Resolve(context.Background(), "acme", v1alpha1.WatchSource{
		Image: &v1alpha1.ImageWatch{
			Repo:           "ghcr.io/acme/app",
			CredentialsRef: &v1alpha1.LocalSecretRef{Name: "absent"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("a missing credentials Secret must be reported by name, got: %v", err)
	}
}
