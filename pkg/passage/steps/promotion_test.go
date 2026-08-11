package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// A promotion, whole: clone the fleet repo, repin the image, commit, push.
//
// The steps are unit-tested apart; this is the only thing that proves they
// compose — that they share a work dir, that an alias reaches the next step's
// expressions, and that the version travels from the Bundle to a commit on the
// remote without anyone spelling it out.
func TestAPromotionEndToEnd(t *testing.T) {
	origin := fleetRepo(t)
	work := t.TempDir()

	bundle := &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo-abc123", Namespace: "acme"},
		Spec: v1alpha1.BundleSpec{
			Beacon: "podinfo",
			Artifacts: []v1alpha1.Artifact{{Image: &v1alpha1.ImageArtifact{
				Repo: "ghcr.io/acme/podinfo", Tag: "6.5.0", Digest: "sha256:abc",
			}}},
		},
	}

	p := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "production-abc123", Namespace: "acme"},
		Spec: v1alpha1.PassageSpec{
			Gate: "production", Bundle: bundle.Name, Actor: "controller",
			Vars: []v1alpha1.Var{{Name: "fleet", Value: origin}},
			Steps: []v1alpha1.Step{
				{Uses: StepGitClone, With: with(t, map[string]any{"repo": "${{ vars.fleet }}"})},
				{Uses: StepSetImage, With: with(t, map[string]any{
					"path":  "repo/production/kustomization.yaml",
					"image": "ghcr.io/acme/podinfo",
				})},
				{Uses: StepGitCommit, As: "commit", With: with(t, map[string]any{
					"message": "promote podinfo ${{ bundle.image('ghcr.io/acme/podinfo').tag }}",
				})},
				{Uses: StepGitPush},
			},
		},
	}

	registry := passage.NewRegistry()
	registry.MustRegister(NewGitClone(nil))
	registry.MustRegister(NewSetImage())
	registry.MustRegister(NewGitCommit())
	registry.MustRegister(NewGitPush(nil))
	engine := &passage.Engine{Registry: registry}

	out := advance(t, engine, p, bundle, work)
	if out.Status.Phase != v1alpha1.PassageSucceeded {
		t.Fatalf("phase = %s: %s", out.Status.Phase, out.Status.Message)
	}

	head := remoteHead(t, origin)
	if !strings.Contains(head.Message, "promote podinfo 6.5.0") {
		t.Errorf("commit message = %q — the Bundle's version did not reach it", head.Message)
	}
	tree, err := head.Tree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := tree.File("production/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "newTag: 6.5.0") {
		t.Errorf("the pushed kustomization was not repinned:\n%s", body)
	}
	// staging must be untouched: a promotion to production that also moves
	// staging is the failure this whole design exists to prevent.
	if s, err := tree.File("staging/kustomization.yaml"); err != nil {
		t.Fatal(err)
	} else if c, _ := s.Contents(); !strings.Contains(c, "newTag: 6.4.0") {
		t.Errorf("staging was changed too:\n%s", c)
	}

	// The alias is what a later flux-wait would wait on.
	if sha := out.Status.Steps[2].Output; sha == nil || !strings.Contains(string(sha.Raw), `"sha"`) {
		t.Error("the commit step published no sha for later steps to wait on")
	}
}

// Re-running the same crossing must converge, not accumulate: the second run
// finds the tag already pinned, the tree already clean, and the commit already
// pushed, and does nothing.
func TestRerunningAPromotionChangesNothing(t *testing.T) {
	origin := fleetRepo(t)
	bundle := &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo-abc123", Namespace: "acme"},
		Spec: v1alpha1.BundleSpec{Beacon: "podinfo", Artifacts: []v1alpha1.Artifact{
			{Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/podinfo", Tag: "6.5.0"}},
		}},
	}
	steps := []v1alpha1.Step{
		{Uses: StepGitClone, With: with(t, map[string]any{"repo": origin})},
		{Uses: StepSetImage, With: with(t, map[string]any{
			"path": "repo/production/kustomization.yaml", "image": "ghcr.io/acme/podinfo",
		})},
		{Uses: StepGitCommit, With: with(t, map[string]any{"message": "promote podinfo"})},
		{Uses: StepGitPush},
	}

	registry := passage.NewRegistry()
	registry.MustRegister(NewGitClone(nil))
	registry.MustRegister(NewSetImage())
	registry.MustRegister(NewGitCommit())
	registry.MustRegister(NewGitPush(nil))
	engine := &passage.Engine{Registry: registry}

	var heads []string
	for i := 0; i < 2; i++ {
		// A fresh work dir each time, because scratch space is disposable (D19)
		// and a retry after a controller restart gets exactly that.
		p := &v1alpha1.Passage{
			ObjectMeta: metav1.ObjectMeta{Name: "production-abc123", Namespace: "acme"},
			Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: bundle.Name, Steps: steps},
		}
		out := advance(t, engine, p, bundle, t.TempDir())
		if out.Status.Phase != v1alpha1.PassageSucceeded {
			t.Fatalf("run %d: phase = %s: %s", i, out.Status.Phase, out.Status.Message)
		}
		heads = append(heads, remoteHead(t, origin).Hash.String())
	}

	if heads[0] != heads[1] {
		t.Errorf("re-running the crossing moved the branch:\n  %s\n  %s", heads[0], heads[1])
	}
}

// advance drives the engine to a terminal phase, as the controller would.
func advance(
	t *testing.T, e *passage.Engine, p *v1alpha1.Passage, b *v1alpha1.Bundle, work string,
) passage.Outcome {
	t.Helper()
	var out passage.Outcome
	for i := 0; i < 20; i++ {
		out = e.Advance(context.Background(), p, b, work)
		p.Status = out.Status
		if out.Status.Phase.Terminal() {
			return out
		}
	}
	t.Fatalf("the Passage never finished: %s", out.Status.Message)
	return out
}

func with(t *testing.T, v map[string]any) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// fleetRepo is a bare repository shaped like a real Flux fleet repo: one
// kustomization per environment, each pinning the same image at its own version.
func fleetRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin, seed := filepath.Join(dir, "fleet.git"), filepath.Join(dir, "seed")

	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	for env, tag := range map[string]string{"staging": "6.4.0", "production": "6.3.0"} {
		writeFile(t, seed, filepath.Join(env, "kustomization.yaml"),
			"# pinned by Hecate\nimages:\n  - name: ghcr.io/acme/podinfo\n    newTag: "+tag+"\n")
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@example.com", When: passageStart.Add(-time.Hour)}
	if _, err := tree.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainClone(origin, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return origin
}

func remoteHead(t *testing.T, origin string) *object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(origin)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return commit
}
