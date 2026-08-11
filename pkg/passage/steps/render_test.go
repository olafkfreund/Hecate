package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// kustomization writes a small but real kustomization into the checkout.
func seedKustomization(t *testing.T, work string) {
	t.Helper()
	writeFile(t, work, "repo/base/deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/acme/api:1.0.0
`)
	writeFile(t, work, "repo/base/configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: api-config
data:
  colour: blue
`)
	writeFile(t, work, "repo/base/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - configmap.yaml
images:
  - name: ghcr.io/acme/api
    newTag: 2.0.0
namePrefix: prod-
`)
}

func TestRenderKustomize(t *testing.T) {
	work := t.TempDir()
	seedKustomization(t, work)

	res := mustRun(t, NewRenderKustomize(), gitCtx(t, work, RenderKustomizeConfig{
		Path: "repo/base", Out: "repo/rendered/base.yaml",
	}))

	rendered := read(t, filepath.Join(work, "repo/rendered/base.yaml"))

	// The build was actually applied, not just the sources concatenated: the
	// image is repinned and the names are prefixed.
	for _, want := range []string{"ghcr.io/acme/api:2.0.0", "prod-api", "prod-api-config"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the render did not apply the kustomization (%q missing):\n%s", want, rendered)
		}
	}
	// Both resources, in one document stream.
	if got := strings.Count(rendered, "\n---\n") + 1; got != 2 {
		t.Errorf("%d documents, want 2:\n%s", got, rendered)
	}
	if res.Output["changed"] != true || res.Output["resources"] != 2 {
		t.Errorf("output = %v", res.Output)
	}
}

// This output is committed. A render that reshuffled between runs would produce
// a diff on every crossing and teach reviewers to skim them — which defeats the
// point of committing rendered manifests at all.
func TestRenderIsDeterministic(t *testing.T) {
	var outputs []string
	for i := 0; i < 3; i++ {
		work := t.TempDir()
		seedKustomization(t, work)
		mustRun(t, NewRenderKustomize(), gitCtx(t, work, RenderKustomizeConfig{
			Path: "repo/base", Out: "repo/out.yaml",
		}))
		outputs = append(outputs, read(t, filepath.Join(work, "repo/out.yaml")))
	}
	for i := 1; i < len(outputs); i++ {
		if outputs[i] != outputs[0] {
			t.Fatalf("run %d rendered differently:\n--- first ---\n%s\n--- later ---\n%s",
				i, outputs[0], outputs[i])
		}
	}
}

// Re-running a crossing must leave the tree clean, or every retry lands an
// empty commit (D23).
func TestRenderIsANoOpWhenUnchanged(t *testing.T) {
	work := t.TempDir()
	seedKustomization(t, work)
	cfg := RenderKustomizeConfig{Path: "repo/base", Out: "repo/out.yaml"}

	mustRun(t, NewRenderKustomize(), gitCtx(t, work, cfg))
	before := read(t, filepath.Join(work, "repo/out.yaml"))
	stat, err := os.Stat(filepath.Join(work, "repo/out.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	res := mustRun(t, NewRenderKustomize(), gitCtx(t, work, cfg))

	if res.Output["changed"] != false {
		t.Error("an unchanged render reported changed=true")
	}
	if read(t, filepath.Join(work, "repo/out.yaml")) != before {
		t.Error("the output was rewritten anyway")
	}
	after, err := os.Stat(filepath.Join(work, "repo/out.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(stat.ModTime()) {
		t.Error("the file was rewritten with identical content, which git would not notice but a watcher would")
	}
}

// A change to the sources must reach the output, or the render is a no-op that
// silently freezes the manifests.
func TestRenderFollowsTheSources(t *testing.T) {
	work := t.TempDir()
	seedKustomization(t, work)
	cfg := RenderKustomizeConfig{Path: "repo/base", Out: "repo/out.yaml"}
	mustRun(t, NewRenderKustomize(), gitCtx(t, work, cfg))

	// What set-image would have done to the kustomization.
	writeFile(t, work, "repo/base/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - configmap.yaml
images:
  - name: ghcr.io/acme/api
    newTag: 3.0.0
namePrefix: prod-
`)
	res := mustRun(t, NewRenderKustomize(), gitCtx(t, work, cfg))

	if res.Output["changed"] != true {
		t.Error("a changed kustomization produced no change")
	}
	if !strings.Contains(read(t, filepath.Join(work, "repo/out.yaml")), "ghcr.io/acme/api:3.0.0") {
		t.Error("the new tag did not reach the rendered output")
	}
}

func TestRenderRefusals(t *testing.T) {
	work := t.TempDir()
	seedKustomization(t, work)
	writeFile(t, work, "repo/broken/kustomization.yaml", "resources:\n  - nonexistent.yaml\n")
	writeFile(t, work, "repo/empty/kustomization.yaml", "resources: []\n")

	for _, tc := range []struct {
		name   string
		cfg    RenderKustomizeConfig
		reason string
		says   string
	}{
		{"a directory that is not there", RenderKustomizeConfig{Path: "repo/ghost", Out: "repo/o.yaml"},
			ReasonFileNotFound, "no directory"},
		{"a kustomization that does not build", RenderKustomizeConfig{Path: "repo/broken", Out: "repo/o.yaml"},
			ReasonRenderFailed, "nonexistent.yaml"},
		{"a build with no resources", RenderKustomizeConfig{Path: "repo/empty", Out: "repo/o.yaml"},
			ReasonNothingRendered, "built no resources"},
		{"no out", RenderKustomizeConfig{Path: "repo/base"}, ReasonInvalidConfig, "required"},
		{"no path", RenderKustomizeConfig{Out: "repo/o.yaml"}, ReasonInvalidConfig, "required"},
		{"a path outside the work dir", RenderKustomizeConfig{Path: "../escape", Out: "repo/o.yaml"},
			ReasonInvalidConfig, "escapes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRenderKustomize().Run(context.Background(), gitCtx(t, work, tc.cfg))
			if !passage.IsTerminal(err) {
				t.Fatalf("err = %v, want a terminal refusal", err)
			}
			if passage.ReasonOf(err) != tc.reason {
				t.Errorf("reason = %s, want %s (%v)", passage.ReasonOf(err), tc.reason, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q: %v", tc.says, err)
			}
		})
	}
}

// The work dir is a scratch directory named after a Passage UID. Quoting it in
// an error tells the reader nothing and hides the path they recognise.
func TestRenderErrorsUseRepositoryPaths(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "repo/broken/kustomization.yaml", "resources:\n  - nonexistent.yaml\n")

	_, err := NewRenderKustomize().Run(context.Background(),
		gitCtx(t, work, RenderKustomizeConfig{Path: "repo/broken", Out: "repo/o.yaml"}))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), work) {
		t.Errorf("the error quotes the scratch directory: %v", err)
	}
	if !strings.Contains(err.Error(), "repo/broken") {
		t.Errorf("the error does not name the repository path: %v", err)
	}
}

// Rendering is the step before the commit, so it has to compose with the rest.
func TestRenderThenCommit(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))
	seedKustomization(t, work)

	mustRun(t, NewRenderKustomize(), gitCtx(t, work, RenderKustomizeConfig{
		Path: "repo/base", Out: "repo/rendered.yaml",
	}))
	res := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "render"}))

	if res.Output["committed"] != true {
		t.Error("the rendered output was not committed")
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Errorf("phase = %s", res.Phase)
	}
}
