package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// A file with the things a real GitOps repository has and a naive round-trip
// destroys: a header comment, a trailing comment, blank lines, quoted and
// unquoted scalars, and four-space indentation.
const valuesYAML = `# Managed by Hecate. Do not hand-edit the tag.
apiVersion: v1

image:
    repository: ghcr.io/acme/api
    tag: 1.0.0            # promoted from staging
    pullPolicy: "IfNotPresent"

replicas: 2
debug: false
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// changedLines reports the lines that differ, which is the property the issue
// is actually about: a promotion must not rewrite the file around its edit.
func changedLines(before, after string) []string {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	var diff []string
	for i := range a {
		if i >= len(b) || b[i] != a[i] {
			diff = append(diff, a[i])
		}
	}
	if len(b) > len(a) {
		diff = append(diff, b[len(a):]...)
	}
	return diff
}

func edits(pairs ...any) []FieldEdit {
	var out []FieldEdit
	for i := 0; i+1 < len(pairs); i += 2 {
		raw, err := json.Marshal(pairs[i+1])
		if err != nil {
			panic(err)
		}
		out = append(out, FieldEdit{Key: pairs[i].(string), Value: raw})
	}
	return out
}

func TestEditYAMLTouchesOnlyTheLineItEdits(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/values.yaml", valuesYAML)

	res := mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
		Path:  "repo/values.yaml",
		Edits: edits("image.tag", "2.0.0"),
	}))

	after := read(t, file)
	diff := changedLines(valuesYAML, after)
	if len(diff) != 1 {
		t.Fatalf("%d lines changed, want 1:\n%s", len(diff), after)
	}
	if !strings.Contains(diff[0], "2.0.0") {
		t.Errorf("changed line = %q", diff[0])
	}
	// The comment says why the value is pinned. Losing it loses the reason.
	if !strings.Contains(diff[0], "# promoted from staging") {
		t.Errorf("the trailing comment was dropped: %q", diff[0])
	}
	if !strings.HasPrefix(diff[0], "    tag:") {
		t.Errorf("indentation was renormalised: %q", diff[0])
	}
	if res.Output["changed"] != true {
		t.Error("a real edit should report changed=true")
	}
}

// `newTag: 1.0` unquoted is a float, and kustomize would render `image:1`.
func TestEditYAMLQuotesWhatWouldChangeType(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"a version that parses as a float", "1.0", `tag: "1.0"`},
		{"a version that does not", "2.0.0", `tag: 2.0.0`},
		{"a word YAML reads as a boolean", "yes", `tag: "yes"`},
		{"the empty string", "", `tag: ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			file := writeFile(t, work, "repo/v.yaml", "tag: 1.0.0\n")

			mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
				Path: "repo/v.yaml", Edits: edits("tag", tc.value),
			}))

			if got := strings.TrimSpace(read(t, file)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEditYAMLKeepsExistingQuotingAndTypes(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/values.yaml", valuesYAML)

	mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
		Path: "repo/values.yaml",
		Edits: edits(
			"image.pullPolicy", "Always", // was double-quoted
			"replicas", 3, // a number stays a number
			"debug", true,
		),
	}))

	after := read(t, file)
	for _, want := range []string{`pullPolicy: "Always"`, "replicas: 3", "debug: true"} {
		if !strings.Contains(after, want) {
			t.Errorf("missing %q in:\n%s", want, after)
		}
	}
}

// Re-running a crossing rewrites the same value. It must produce no diff, or
// every retry lands an empty commit (D23).
func TestEditYAMLIsANoOpWhenAlreadyCorrect(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/values.yaml", valuesYAML)
	cfg := EditYAMLConfig{Path: "repo/values.yaml", Edits: edits("image.tag", "1.0.0")}

	res := mustRun(t, NewEditYAML(), gitCtx(t, work, cfg))

	if res.Output["changed"] != false {
		t.Error("an edit that changes nothing should report changed=false")
	}
	if read(t, file) != valuesYAML {
		t.Errorf("the file was rewritten anyway:\n%s", read(t, file))
	}
}

func TestEditYAMLPaths(t *testing.T) {
	const doc = `images:
  - name: ghcr.io/acme/api
    newTag: 1.0.0
metadata:
  annotations:
    flux.weave.works/tag: v1
`
	for _, tc := range []struct{ name, key, want string }{
		{"sequence index", "images[0].newTag", "newTag: 2.0.0"},
		{"escaped dot in a key", `metadata.annotations.flux\.weave\.works/tag`, "flux.weave.works/tag: 2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			file := writeFile(t, work, "repo/k.yaml", doc)

			mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
				Path: "repo/k.yaml", Edits: edits(tc.key, "2.0.0"),
			}))

			if !strings.Contains(read(t, file), tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, read(t, file))
			}
		})
	}
}

// JSON is YAML, and replacing a scalar in place keeps it valid JSON — so there
// is no separate edit-json step to write or to keep in sync.
func TestEditYAMLEditsJSONInPlace(t *testing.T) {
	const doc = "{\n  \"name\": \"api\",\n  \"version\": \"1.0.0\"\n}\n"
	work := t.TempDir()
	file := writeFile(t, work, "repo/package.json", doc)

	mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
		Path: "repo/package.json", Edits: edits("version", "2.0.0"),
	}))

	after := read(t, file)
	var parsed map[string]string
	if err := json.Unmarshal([]byte(after), &parsed); err != nil {
		t.Fatalf("no longer valid JSON: %v\n%s", err, after)
	}
	if parsed["version"] != "2.0.0" {
		t.Errorf("version = %q", parsed["version"])
	}
}

func TestEditYAMLMultiDocument(t *testing.T) {
	const doc = `kind: ConfigMap
data:
  colour: blue
---
kind: Deployment
replicas: 1
`
	work := t.TempDir()
	file := writeFile(t, work, "repo/m.yaml", doc)

	// A path unique to the second document is found there.
	mustRun(t, NewEditYAML(), gitCtx(t, work, EditYAMLConfig{
		Path: "repo/m.yaml", Edits: edits("replicas", 4),
	}))
	if !strings.Contains(read(t, file), "replicas: 4") {
		t.Errorf("second document not edited:\n%s", read(t, file))
	}

	// A path present in both is ambiguous, and guessing would edit the wrong
	// object.
	_, err := NewEditYAML().Run(context.Background(), gitCtx(t, work, EditYAMLConfig{
		Path: "repo/m.yaml", Edits: edits("kind", "Job"),
	}))
	if !passage.IsTerminal(err) || !strings.Contains(err.Error(), "more than one document") {
		t.Errorf("ambiguous path: err = %v", err)
	}
}

func TestEditYAMLRefusals(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "repo/values.yaml", valuesYAML)
	writeFile(t, work, "repo/block.yaml", "script: |\n  echo hello\n")

	for _, tc := range []struct {
		name   string
		cfg    EditYAMLConfig
		reason string
	}{
		{"a missing file", EditYAMLConfig{Path: "repo/nope.yaml", Edits: edits("a", "b")}, ReasonFileNotFound},
		{"a missing key", EditYAMLConfig{Path: "repo/values.yaml", Edits: edits("image.digest", "x")}, ReasonKeyNotFound},
		{"a key under a scalar", EditYAMLConfig{Path: "repo/values.yaml", Edits: edits("replicas.deeper", "x")}, ReasonKeyNotFound},
		{"a whole mapping", EditYAMLConfig{Path: "repo/values.yaml", Edits: edits("image", "x")}, ReasonEditFailed},
		{"a block scalar", EditYAMLConfig{Path: "repo/block.yaml", Edits: edits("script", "echo bye")}, ReasonEditFailed},
		{"no path", EditYAMLConfig{Edits: edits("a", "b")}, ReasonInvalidConfig},
		{"no edits", EditYAMLConfig{Path: "repo/values.yaml"}, ReasonInvalidConfig},
		{"a path outside the work dir", EditYAMLConfig{Path: "../escape.yaml", Edits: edits("a", "b")}, ReasonInvalidConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEditYAML().Run(context.Background(), gitCtx(t, work, tc.cfg))
			if !passage.IsTerminal(err) {
				t.Fatalf("err = %v, want a terminal failure", err)
			}
			if got := passage.ReasonOf(err); got != tc.reason {
				t.Errorf("reason = %s, want %s (%v)", got, tc.reason, err)
			}
		})
	}
}

// Nothing may be written when an edit is refused: a file half-edited by a step
// that then failed is worse than one not edited at all.
func TestEditYAMLLeavesTheFileAloneOnFailure(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/values.yaml", valuesYAML)

	_, err := NewEditYAML().Run(context.Background(), gitCtx(t, work, EditYAMLConfig{
		Path: "repo/values.yaml",
		Edits: edits(
			"image.tag", "2.0.0", // fine
			"image.nope", "x", // not there
		),
	}))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if read(t, file) != valuesYAML {
		t.Errorf("the first edit was written despite the second failing:\n%s", read(t, file))
	}
}

// ------------------------------------------------------------- set-image ----

const kustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
images:
  - name: ghcr.io/acme/sidecar
    newTag: 0.9.0
  - name: ghcr.io/acme/api
    newTag: 1.0.0
`

func bundleCtx(t *testing.T, work string, cfg any, artifacts ...v1alpha1.Artifact) *passage.StepContext {
	t.Helper()
	sc := gitCtx(t, work, cfg)
	sc.Bundle = &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo-abc123", Namespace: "acme"},
		Spec:       v1alpha1.BundleSpec{Beacon: "podinfo", Artifacts: artifacts},
	}
	return sc
}

// The entry is found by the repository it names. Addressing it by index would
// repin the sidecar the day somebody reorders the list.
func TestSetImageFindsTheEntryByName(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/kustomization.yaml", kustomizationYAML)

	res := mustRun(t, NewSetImage(), gitCtx(t, work, SetImageConfig{
		Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/api", Tag: "2.0.0",
	}))

	after := read(t, file)
	if !strings.Contains(after, "newTag: 2.0.0") {
		t.Errorf("api was not repinned:\n%s", after)
	}
	if !strings.Contains(after, "newTag: 0.9.0") {
		t.Errorf("the sidecar was repinned too:\n%s", after)
	}
	if len(changedLines(kustomizationYAML, after)) != 1 {
		t.Errorf("more than the pinned line changed:\n%s", after)
	}
	if res.Output["pinned"] != "2.0.0" {
		t.Errorf("output pinned = %v", res.Output["pinned"])
	}
}

// The usual case: the Gate says which image, the Bundle says which version.
func TestSetImageTakesTheVersionFromTheBundle(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/kustomization.yaml", kustomizationYAML)

	mustRun(t, NewSetImage(), bundleCtx(t, work,
		SetImageConfig{Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/api"},
		v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{
			Repo: "ghcr.io/acme/api", Tag: "3.1.4", Digest: "sha256:abc",
		}},
	))

	if !strings.Contains(read(t, file), "newTag: 3.1.4") {
		t.Errorf("not repinned from the Bundle:\n%s", read(t, file))
	}
}

// Switching a tag pin to a digest pin changes the file's shape, so the step
// writes whichever field the entry already uses.
func TestSetImageWritesTheFieldTheEntryUses(t *testing.T) {
	const digestPinned = `images:
  - name: ghcr.io/acme/api
    digest: sha256:0000
`
	work := t.TempDir()
	file := writeFile(t, work, "repo/kustomization.yaml", digestPinned)

	mustRun(t, NewSetImage(), bundleCtx(t, work,
		SetImageConfig{Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/api"},
		v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{
			Repo: "ghcr.io/acme/api", Tag: "3.1.4", Digest: "sha256:beef",
		}},
	))

	after := read(t, file)
	if !strings.Contains(after, "digest: sha256:beef") {
		t.Errorf("digest not updated:\n%s", after)
	}
	if strings.Contains(after, "newTag") {
		t.Errorf("a newTag field was invented:\n%s", after)
	}
}

func TestSetImageIsANoOpWhenAlreadyPinned(t *testing.T) {
	work := t.TempDir()
	file := writeFile(t, work, "repo/kustomization.yaml", kustomizationYAML)

	res := mustRun(t, NewSetImage(), gitCtx(t, work, SetImageConfig{
		Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/api", Tag: "1.0.0",
	}))

	if res.Output["changed"] != false {
		t.Error("repinning to the current version should report changed=false")
	}
	if read(t, file) != kustomizationYAML {
		t.Error("the file was rewritten anyway")
	}
}

func TestSetImageRefusals(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "repo/kustomization.yaml", kustomizationYAML)
	writeFile(t, work, "repo/unpinned.yaml", "images:\n  - name: ghcr.io/acme/api\n")

	t.Run("an image the file does not pin", func(t *testing.T) {
		_, err := NewSetImage().Run(context.Background(), gitCtx(t, work, SetImageConfig{
			Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/ghost", Tag: "1.0.0",
		}))
		if passage.ReasonOf(err) != ReasonKeyNotFound {
			t.Errorf("reason = %s (%v)", passage.ReasonOf(err), err)
		}
	})

	t.Run("an entry with nothing to update", func(t *testing.T) {
		_, err := NewSetImage().Run(context.Background(), gitCtx(t, work, SetImageConfig{
			Path: "repo/unpinned.yaml", Image: "ghcr.io/acme/api", Tag: "1.0.0",
		}))
		if passage.ReasonOf(err) != ReasonKeyNotFound {
			t.Errorf("reason = %s (%v)", passage.ReasonOf(err), err)
		}
	})

	t.Run("an image the Bundle does not carry", func(t *testing.T) {
		_, err := NewSetImage().Run(context.Background(), bundleCtx(t, work,
			SetImageConfig{Path: "repo/kustomization.yaml", Image: "ghcr.io/acme/api"},
			v1alpha1.Artifact{Image: &v1alpha1.ImageArtifact{Repo: "ghcr.io/acme/other", Tag: "1.0.0"}},
		))
		if passage.ReasonOf(err) != ReasonInvalidConfig {
			t.Errorf("reason = %s (%v)", passage.ReasonOf(err), err)
		}
	})

	t.Run("no image", func(t *testing.T) {
		_, err := NewSetImage().Run(context.Background(), gitCtx(t, work, SetImageConfig{
			Path: "repo/kustomization.yaml", Tag: "1.0.0",
		}))
		if passage.ReasonOf(err) != ReasonInvalidConfig {
			t.Errorf("reason = %s (%v)", passage.ReasonOf(err), err)
		}
	})
}
