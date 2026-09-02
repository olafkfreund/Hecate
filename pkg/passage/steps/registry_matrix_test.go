//go:build registrymatrix

// Registry matrix: the OCI steps against a real registry rather than one
// running in the test process (#50).
//
// The in-process registry proves the steps' logic. It cannot prove the thing
// that actually breaks in the field, which is authentication — every registry
// has its own idea of what a credential is, and go-containerregistry's keychain
// covers them by different routes. So this drives the same push-and-pull the
// unit tests do, against whatever HECATE_REGISTRY_REPO names, and CI points it
// at one registry per job.
//
//	HECATE_REGISTRY_REPO=ghcr.io/olafkfreund/hecate-ci \
//	  go test -tags registrymatrix -count=1 -v -run Registry ./pkg/passage/steps/
package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/olafkfreund/hecate/pkg/registry"
)

// matrixRepo is the repository under test, or a skip.
func matrixRepo(t *testing.T) (repo string, insecure bool) {
	t.Helper()
	repo = os.Getenv("HECATE_REGISTRY_REPO")
	if repo == "" {
		t.Skip("set HECATE_REGISTRY_REPO to run the registry matrix")
	}
	// A local Harbor or registry:2 in CI terminates no TLS.
	return repo, os.Getenv("HECATE_REGISTRY_INSECURE") == "true"
}

// A round trip through a real registry: push a rendered tree, pull it back,
// and check the files survived.
//
// The tag carries the run id so concurrent jobs and re-runs do not collide on
// a mutable tag, which several registries treat differently.
func TestRegistryMatrixPushAndPull(t *testing.T) {
	repo, insecure := matrixRepo(t)
	tag := os.Getenv("HECATE_REGISTRY_TAG")
	if tag == "" {
		tag = "ci-" + time.Now().UTC().Format("20060102-150405")
	}

	work := t.TempDir()
	seedManifests(t, work)

	res := mustRun(t, NewOCIPush(nil), gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: repo, Tag: tag,
		Source: "https://github.com/olafkfreund/hecate", Revision: "ci",
		Insecure: insecure,
	}))
	digest, _ := res.Output["digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q", digest)
	}
	t.Logf("pushed %s:%s -> %s", repo, tag, digest)

	pulled := t.TempDir()
	pull := mustRun(t, NewOCIPull(nil), gitCtx(t, pulled, OCIPullConfig{
		Repo: repo, Tag: tag, Out: "repo/fetched", Insecure: insecure,
	}))
	if pull.Output["files"] != 2 {
		t.Errorf("pulled %v files, want 2", pull.Output["files"])
	}
	for _, p := range []string{"repo/fetched/deployment.yaml", "repo/fetched/nested/configmap.yaml"} {
		if got := read(t, filepath.Join(pulled, p)); !strings.Contains(got, "apiVersion") {
			t.Errorf("%s did not come back: %q", p, got)
		}
	}

	// By digest as well as by tag. A Beacon resolves digests, and some
	// registries answer the two differently — Docker Hub in particular.
	byDigest := t.TempDir()
	d := mustRun(t, NewOCIPull(nil), gitCtx(t, byDigest, OCIPullConfig{
		Repo: repo, Digest: digest, Out: "repo/fetched", Insecure: insecure,
	}))
	if d.Output["files"] != 2 {
		t.Errorf("pulled %v files by digest, want 2", d.Output["files"])
	}
}

// Where the credentials actually come from, which is the part the in-process
// registry can never test.
//
// pkg/registry used to claim its chain covered "the cloud keychains that cover
// IRSA, Workload Identity and Managed Identity" while actually building a bare
// authn.DefaultKeychain, which reads ~/.docker/config.json and any credential
// helper named there and nothing else. D56 made that true for ECR and
// GCR/Artifact Registry by composing real cloud keychains into the chain — so
// this test's original premise, that no ambient credential exists anywhere, no
// longer holds for those two.
//
// It still holds for everything else: a registry ambientKeychain does not
// recognise (ghcr.io, Docker Hub, a private Harbor) must still fail without a
// docker config, exactly as before. And it still cannot be *proven* for ECR or
// GCR here — that needs a real EKS or GKE runner with actual workload
// identity, which no GitHub Actions job has (#50, D56).
func TestRegistryMatrixNeedsAmbientDockerCredentials(t *testing.T) {
	repo, insecure := matrixRepo(t)
	if os.Getenv("HECATE_REGISTRY_ANONYMOUS") == "true" {
		t.Skip("this registry accepts anonymous pushes, so there is nothing to prove")
	}
	if host := repoHost(repo); registry.IsAmbientCloudRegistry(host) {
		t.Skipf("%s has ambient cloud keychain support (D56); proving it needs a real "+
			"AWS/GCP runner with workload identity, not a docker-config check — see #50", host)
	}

	// An empty DOCKER_CONFIG is what a controller pod looks like.
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	work := t.TempDir()
	seedManifests(t, work)
	sc := gitCtx(t, work, OCIPushConfig{
		Path: "repo/rendered", Repo: repo, Tag: "should-not-exist",
		Insecure: insecure,
	})
	_, err := NewOCIPush(nil).Run(context.Background(), sc)
	if err == nil {
		t.Fatal("the push succeeded with no docker config — Hecate has grown ambient " +
			"cloud credentials, which is good; update pkg/registry's doc comment and rewrite this test")
	}
	t.Logf("refused without ambient credentials, as expected: %v", err)
}

// repoHost extracts the registry host from HECATE_REGISTRY_REPO, e.g.
// "ghcr.io" from "ghcr.io/olafkfreund/hecate-ci".
func repoHost(repo string) string {
	ref, err := name.NewRepository(repo)
	if err != nil {
		return repo
	}
	return ref.RegistryStr()
}
