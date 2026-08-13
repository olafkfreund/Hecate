package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// realExports is what a ResourceSetInputProvider actually exported, captured
// from one watching oci://ghcr.io/stefanprodan/charts/podinfo with
// `semver: ">=6.0.0"` in the dev cluster. Copied rather than composed: a
// fixture written from the same reading of the source as the code proves only
// that the reading is self-consistent.
const realExports = `[
  {"digest":"sha256:6432072d76519fd808e4fb6eb892f9cbcbb1a79c161ac02dbf6ad445c931616b","id":"69009705","tag":"6.14.1"},
  {"digest":"sha256:476bed61733536f99e7331b0fe4cc9fd70bc6497a855ad38ba49b72de50c1132","id":"68944168","tag":"6.14.0"},
  {"digest":"sha256:63181192ad277784a174b3aeec18284cc924e71b2d0af02584e3fc0664407489","id":"68747559","tag":"6.13.0"}
]`

func providerObj(t *testing.T, name, url, exports string, ready bool) *unstructured.Unstructured {
	t.Helper()
	var inputs []any
	if exports != "" {
		if err := json.Unmarshal([]byte(exports), &inputs); err != nil {
			t.Fatal(err)
		}
	}
	status := "False"
	if ready {
		status = "True"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "fluxcd.controlplane.io/v1",
		"kind":       "ResourceSetInputProvider",
		"metadata":   map[string]any{"name": name, "namespace": "acme"},
		"spec":       map[string]any{"type": "OCIArtifactTag", "url": url},
		"status": map[string]any{
			"exportedInputs": inputs,
			"conditions": []any{map[string]any{
				"type": "Ready", "status": status,
				"reason": "Failed", "message": "the registry refused the credentials",
			}},
		},
	}}
	obj.SetGroupVersionKind(providerGVK)
	return obj
}

func providerResolver(objs ...*unstructured.Unstructured) *Resolver {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &Resolver{Client: b.Build()}
}

func resolveProviderWatch(t *testing.T, r *Resolver, name string) (v1alpha1.Artifact, error) {
	t.Helper()
	return r.Resolve(context.Background(), "acme",
		v1alpha1.WatchSource{Provider: &v1alpha1.ProviderWatch{Name: name}})
}

// The heart of #23: the provider has already filtered and sorted, so the first
// exported input is the newest. Running the list through Hecate's own Selection
// would be a second opinion about the same question, from different rules.
func TestAProviderWatchTakesTheNewestExportedInput(t *testing.T) {
	r := providerResolver(providerObj(t, "podinfo",
		"oci://ghcr.io/stefanprodan/charts/podinfo", realExports, true))

	got, err := resolveProviderWatch(t, r, "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Image == nil {
		t.Fatalf("resolved to %+v, want an image", got)
	}
	if got.Image.Tag != "6.14.1" {
		t.Errorf("tag = %q, want 6.14.1 — the provider sorted these already", got.Image.Tag)
	}
	// A digest is what makes a promotion auditable, and the provider gives one.
	if !strings.HasPrefix(got.Image.Digest, "sha256:") {
		t.Errorf("digest = %q", got.Image.Digest)
	}
	// oci:// is how a provider names a registry and not how an image reference
	// does; left on, it would leak into every rendered manifest.
	if got.Image.Repo != "ghcr.io/stefanprodan/charts/podinfo" {
		t.Errorf("repo = %q, want the oci:// scheme stripped", got.Image.Repo)
	}
}

// Its inputs are then stale or absent, and a Bundle minted from stale inputs
// pins the wrong artifact while looking exactly like a correct one.
func TestAProviderThatIsNotReadyIsRefused(t *testing.T) {
	r := providerResolver(providerObj(t, "podinfo",
		"oci://ghcr.io/acme/app", realExports, false))

	_, err := resolveProviderWatch(t, r, "podinfo")
	var noMatch *ErrNoMatch
	if !errors.As(err, &noMatch) {
		t.Fatalf("err = %T %v, want ErrNoMatch", err, err)
	}
	// The provider's own message, not ours: it knows why it failed.
	if !strings.Contains(err.Error(), "refused the credentials") {
		t.Errorf("error = %v, want the provider's own reason", err)
	}
}

// Ready and empty is a real state — every tag was filtered out. The watch is
// correctly configured and simply has nothing to offer yet.
func TestAProviderWithNoInputsIsNotAnError(t *testing.T) {
	r := providerResolver(providerObj(t, "podinfo", "oci://ghcr.io/acme/app", "[]", true))

	_, err := resolveProviderWatch(t, r, "podinfo")
	var noMatch *ErrNoMatch
	if !errors.As(err, &noMatch) {
		t.Fatalf("err = %T %v, want ErrNoMatch", err, err)
	}
}

// The git providers export a sha, which is what a promotion pins.
func TestAGitProviderResolvesToACommit(t *testing.T) {
	const gitExports = `[{"id":"42","sha":"1c3f5a7b9d1e3f5a7b9d1e3f5a7b9d1e3f5a7b9d",
	  "branch":"main","author":"olaf","title":"promote podinfo"}]`
	r := providerResolver(providerObj(t, "app",
		"https://github.com/acme/app", gitExports, true))

	got, err := resolveProviderWatch(t, r, "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit == nil {
		t.Fatalf("resolved to %+v, want a commit", got)
	}
	if got.Commit.SHA != "1c3f5a7b9d1e3f5a7b9d1e3f5a7b9d1e3f5a7b9d" || got.Commit.Branch != "main" {
		t.Errorf("commit = %+v", got.Commit)
	}
	if got.Commit.Repo != "https://github.com/acme/app" {
		t.Errorf("repo = %q", got.Commit.Repo)
	}
}

// A Static provider exports whatever its author chose, which may be nothing
// promotable. Better said plainly than turned into an empty Artifact that
// fails later with less to go on.
func TestAnInputWithNothingToPromoteSaysSo(t *testing.T) {
	r := providerResolver(providerObj(t, "static",
		"", `[{"id":"1","env":"staging","replicas":"3"}]`, true))

	_, err := resolveProviderWatch(t, r, "static")
	if err == nil {
		t.Fatal("an input with neither sha nor tag resolved to something")
	}
	// Names what it did carry, sorted, so the message is actionable and stable.
	if !strings.Contains(err.Error(), "[env id replicas]") {
		t.Errorf("error = %v, want the keys it did export", err)
	}
}

func TestAMissingProviderIsReported(t *testing.T) {
	r := providerResolver()
	if _, err := resolveProviderWatch(t, r, "nope"); err == nil {
		t.Error("a watch on a provider that does not exist resolved to something")
	}
}
