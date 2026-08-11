package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olafkfreund/hecate/pkg/passage"
)

// seedChart writes a small but real chart into the checkout.
func seedChart(t *testing.T, work string) {
	t.Helper()
	writeFile(t, work, "repo/chart/Chart.yaml", `apiVersion: v2
name: api
version: 0.1.0
appVersion: "1.0.0"
`)
	writeFile(t, work, "repo/chart/values.yaml", `replicaCount: 1
image:
  repository: ghcr.io/acme/api
  tag: "1.0.0"
config:
  colour: blue
  size: small
`)
	writeFile(t, work, "repo/chart/templates/deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: {{ .Values.replicaCount }}
  template:
    spec:
      containers:
        - name: api
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`)
	writeFile(t, work, "repo/chart/templates/configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  colour: {{ .Values.config.colour | quote }}
  size: {{ .Values.config.size | quote }}
`)
}

func helmCfg(opts ...func(*RenderHelmConfig)) RenderHelmConfig {
	cfg := RenderHelmConfig{
		Chart: "repo/chart", Out: "repo/rendered.yaml", ReleaseName: "api",
	}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

func TestRenderHelm(t *testing.T) {
	work := t.TempDir()
	seedChart(t, work)

	res := mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg()))

	rendered := read(t, filepath.Join(work, "repo/rendered.yaml"))
	for _, want := range []string{
		"ghcr.io/acme/api:1.0.0", // values applied
		"name: api",              // .Release.Name
		"namespace: acme",        // .Release.Namespace, from the Gate's namespace
		"colour: \"blue\"",       // a second template
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the render is missing %q:\n%s", want, rendered)
		}
	}
	if res.Output["changed"] != true {
		t.Errorf("output = %v", res.Output)
	}
}

// This output is committed, so a render that reordered between runs would
// produce a diff on every crossing.
func TestRenderHelmIsDeterministic(t *testing.T) {
	var outputs []string
	for i := 0; i < 3; i++ {
		work := t.TempDir()
		seedChart(t, work)
		mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg()))
		outputs = append(outputs, read(t, filepath.Join(work, "repo/rendered.yaml")))
	}
	for i := 1; i < len(outputs); i++ {
		if outputs[i] != outputs[0] {
			t.Fatalf("run %d rendered differently:\n--- first ---\n%s\n--- later ---\n%s",
				i, outputs[0], outputs[i])
		}
	}
}

// Later values win, which is Helm's own ordering — a step can override a file
// without rewriting it.
func TestRenderHelmValuePrecedence(t *testing.T) {
	work := t.TempDir()
	seedChart(t, work)
	writeFile(t, work, "repo/prod.yaml", "replicaCount: 5\nconfig:\n  colour: red\n")

	inline := json.RawMessage(`{"image":{"tag":"2.0.0"},"config":{"size":"large"}}`)
	mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg(func(c *RenderHelmConfig) {
		c.ValuesFiles = []string{"repo/prod.yaml"}
		c.Values = &inline
	})))

	rendered := read(t, filepath.Join(work, "repo/rendered.yaml"))
	for _, want := range []string{
		"replicas: 5",            // from the values file
		"ghcr.io/acme/api:2.0.0", // inline beat the chart default
		`colour: "red"`,          // file beat the chart default
		`size: "large"`,          // inline, and the map was merged rather than replaced
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q:\n%s", want, rendered)
		}
	}
}

// Rendering must not depend on which cluster the controller happens to run in,
// or the same commit would render differently in two places.
func TestRenderHelmCapabilitiesAreConfigured(t *testing.T) {
	work := t.TempDir()
	seedChart(t, work)
	writeFile(t, work, "repo/chart/templates/version.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-version
data:
  kube: {{ .Capabilities.KubeVersion.Version | quote }}
`)

	mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg(func(c *RenderHelmConfig) {
		c.KubeVersion = "v1.31.0"
	})))

	if got := read(t, filepath.Join(work, "repo/rendered.yaml")); !strings.Contains(got, "v1.31.0") {
		t.Errorf("the configured KubeVersion did not reach the template:\n%s", got)
	}
}

func TestRenderHelmIsANoOpWhenUnchanged(t *testing.T) {
	work := t.TempDir()
	seedChart(t, work)

	mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg()))
	before := read(t, filepath.Join(work, "repo/rendered.yaml"))
	res := mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg()))

	if res.Output["changed"] != false {
		t.Error("an unchanged render reported changed=true")
	}
	if read(t, filepath.Join(work, "repo/rendered.yaml")) != before {
		t.Error("the output was rewritten anyway")
	}
}

func TestRenderHelmRefusals(t *testing.T) {
	work := t.TempDir()
	seedChart(t, work)
	writeFile(t, work, "repo/broken/Chart.yaml", "apiVersion: v2\nname: broken\nversion: 0.1.0\n")
	writeFile(t, work, "repo/broken/templates/bad.yaml", "kind: {{ .Values.missing.deeply }}\n")

	for _, tc := range []struct {
		name   string
		cfg    RenderHelmConfig
		reason string
		says   string
	}{
		{"a chart that is not there",
			helmCfg(func(c *RenderHelmConfig) { c.Chart = "repo/ghost" }),
			ReasonFileNotFound, "no chart"},
		{"a template that does not render",
			helmCfg(func(c *RenderHelmConfig) { c.Chart = "repo/broken" }),
			ReasonRenderFailed, "bad.yaml"},
		{"a values file that is not there",
			helmCfg(func(c *RenderHelmConfig) { c.ValuesFiles = []string{"repo/ghost.yaml"} }),
			ReasonFileNotFound, "no values file"},
		{"no release name",
			helmCfg(func(c *RenderHelmConfig) { c.ReleaseName = "" }),
			ReasonInvalidConfig, "releaseName is required"},
		{"no out",
			helmCfg(func(c *RenderHelmConfig) { c.Out = "" }),
			ReasonInvalidConfig, "out is required"},
		{"a chart outside the work dir",
			helmCfg(func(c *RenderHelmConfig) { c.Chart = "../escape" }),
			ReasonInvalidConfig, "escapes"},
		{"an unusable kubeVersion",
			helmCfg(func(c *RenderHelmConfig) { c.KubeVersion = "not-a-version" }),
			ReasonInvalidConfig, "kubeVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRenderHelm().Run(context.Background(), gitCtx(t, work, tc.cfg))
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

// The work dir is a scratch directory named after a Passage UID; quoting it
// hides the path the author recognises.
func TestRenderHelmErrorsUseRepositoryPaths(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "repo/broken/Chart.yaml", "apiVersion: v2\nname: broken\nversion: 0.1.0\n")
	writeFile(t, work, "repo/broken/templates/bad.yaml", "kind: {{ .Values.missing.deeply }}\n")

	_, err := NewRenderHelm().Run(context.Background(),
		gitCtx(t, work, helmCfg(func(c *RenderHelmConfig) { c.Chart = "repo/broken" })))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), work) {
		t.Errorf("the error quotes the scratch directory: %v", err)
	}
}

// Rendering is the step before the commit, so it has to compose.
func TestRenderHelmThenCommit(t *testing.T) {
	origin, work := originRepo(t), t.TempDir()
	mustRun(t, NewGitClone(nil), gitCtx(t, work, GitCloneConfig{Repo: origin}))
	seedChart(t, work)

	mustRun(t, NewRenderHelm(), gitCtx(t, work, helmCfg()))
	res := mustRun(t, NewGitCommit(), gitCtx(t, work, GitCommitConfig{Message: "render"}))

	if res.Output["committed"] != true {
		t.Error("the rendered chart was not committed")
	}
}
