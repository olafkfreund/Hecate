package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepRenderHelm is the value used in `steps[].uses`.
const StepRenderHelm = "render-helm"

// RenderHelmConfig is the `with:` block of a render-helm step.
type RenderHelmConfig struct {
	// Chart is the chart directory, relative to the Passage work dir.
	//
	// A directory in the checkout, not a repository reference: what is rendered
	// has to be what was reviewed, and a chart pulled at render time could
	// differ between the pull request and the crossing.
	Chart string `json:"chart"`
	// Out is where to write the rendered YAML, relative to the work dir.
	Out string `json:"out"`
	// ValuesFiles are values files to apply in order, relative to the work dir.
	ValuesFiles []string `json:"valuesFiles,omitempty"`
	// Values are inline values, applied last so a step can override a file.
	Values *apiextensionsJSON `json:"values,omitempty"`
	// ReleaseName is the release the templates see as .Release.Name.
	ReleaseName string `json:"releaseName"`
	// Namespace is what the templates see as .Release.Namespace. Empty uses the
	// Gate's own namespace, which is the usual intent.
	Namespace string `json:"namespace,omitempty"`
	// IncludeCRDs renders the chart's crds/ directory too. Off by default,
	// matching `helm template`.
	IncludeCRDs bool `json:"includeCRDs,omitempty"`
	// KubeVersion is what .Capabilities.KubeVersion reports. Empty uses Helm's
	// default rather than the cluster's: rendering must not depend on which
	// cluster the controller happens to be running in, or the same commit would
	// render differently in two places.
	KubeVersion string `json:"kubeVersion,omitempty"`
	// APIVersions is what .Capabilities.APIVersions reports, for charts that
	// branch on whether an API is available.
	APIVersions []string `json:"apiVersions,omitempty"`
}

// apiextensionsJSON is an alias so the config can carry arbitrary values
// without importing the apiextensions package into every reader of this file.
type apiextensionsJSON = json.RawMessage

// RenderHelm templates a chart and writes the result into the checkout.
//
// Templating only. It never talks to a cluster, never installs, and never reads
// release history: `.Capabilities` comes from configuration rather than from
// wherever the controller happens to be running, so the same commit renders the
// same bytes anywhere. That is what makes the output safe to commit (D36).
type RenderHelm struct{}

// NewRenderHelm returns a render-helm step.
func NewRenderHelm() *RenderHelm { return &RenderHelm{} }

// Name implements passage.Runner.
func (r *RenderHelm) Name() string { return StepRenderHelm }

// Run implements passage.Runner.
func (r *RenderHelm) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[RenderHelmConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
	}
	if err := cfg.check(); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
	}

	chartDir, err := checkoutPath(sc.WorkDir, cfg.Chart)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
	}
	out, err := checkoutPath(sc.WorkDir, cfg.Out)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
	}
	if _, err := os.Stat(chartDir); os.IsNotExist(err) {
		return passage.StepResult{}, passage.FailTerminal(ReasonFileNotFound,
			"%s: no chart at %s — check the path, and that a git-clone step ran first",
			StepRenderHelm, cfg.Chart)
	}

	chart, err := loader.Load(chartDir)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: loading the chart at %s: %s", StepRenderHelm, cfg.Chart, tidyErr(err, sc.WorkDir))
	}

	values, err := r.values(sc.WorkDir, cfg)
	if err != nil {
		return passage.StepResult{}, err
	}

	namespace := cfg.Namespace
	if namespace == "" {
		namespace = sc.Namespace
	}

	install := action.NewInstall(&action.Configuration{})
	// Client-side only: no cluster is contacted, nothing is installed, and no
	// release history is read or written.
	install.DryRunStrategy = action.DryRunClient
	install.ReleaseName = cfg.ReleaseName
	install.Namespace = namespace
	install.IncludeCRDs = cfg.IncludeCRDs
	install.DisableHooks = true
	if err := applyCapabilities(install, cfg); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
	}

	result, err := install.RunWithContext(ctx, chart, values)
	if err != nil {
		// A chart that does not template will not template on the next attempt.
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: %s", StepRenderHelm, tidyErr(err, sc.WorkDir))
	}

	rendered := manifestOf(result)
	if strings.TrimSpace(rendered) == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonNothingRendered,
			"%s: %s rendered nothing — every template may be behind a disabled condition",
			StepRenderHelm, cfg.Chart)
	}
	if !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}

	return writeRendered(out, cfg.Out, []byte(rendered), StepRenderHelm,
		fmt.Sprintf("rendered %s as %s", cfg.Chart, cfg.ReleaseName))
}

// values merges the values files in order, then the inline values.
//
// Later wins, which is Helm's own ordering, so a step can override a file
// without rewriting it.
func (r *RenderHelm) values(workDir string, cfg RenderHelmConfig) (map[string]any, error) {
	merged := map[string]any{}

	for _, name := range cfg.ValuesFiles {
		path, err := checkoutPath(workDir, name)
		if err != nil {
			return nil, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderHelm, err)
		}
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil, passage.FailTerminal(ReasonFileNotFound,
				"%s: no values file at %s", StepRenderHelm, name)
		}
		if err != nil {
			return nil, passage.FailTerminal(ReasonRenderFailed,
				"%s: reading %s: %s", StepRenderHelm, name, err)
		}
		var parsed map[string]any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			return nil, passage.FailTerminal(ReasonRenderFailed,
				"%s: %s is not valid YAML: %s", StepRenderHelm, name, err)
		}
		merged = mergeValues(merged, parsed)
	}

	if cfg.Values != nil && len(*cfg.Values) > 0 {
		var inline map[string]any
		if err := json.Unmarshal(*cfg.Values, &inline); err != nil {
			return nil, passage.FailTerminal(ReasonInvalidConfig,
				"%s: values must be an object: %s", StepRenderHelm, err)
		}
		merged = mergeValues(merged, inline)
	}
	return merged, nil
}

// mergeValues merges src into dst, recursing into maps.
//
// Helm's own semantics: a map is merged key by key, and anything else replaces.
// A list replaces rather than appends, which surprises people but is what
// `helm template -f a.yaml -f b.yaml` does, and matching it matters more than
// being intuitive.
func mergeValues(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if nested, ok := v.(map[string]any); ok {
			if existing, ok := out[k].(map[string]any); ok {
				out[k] = mergeValues(existing, nested)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// applyCapabilities sets what the chart sees as the cluster's capabilities.
func applyCapabilities(install *action.Install, cfg RenderHelmConfig) error {
	if cfg.KubeVersion != "" {
		parsed, err := common.ParseKubeVersion(cfg.KubeVersion)
		if err != nil {
			return fmt.Errorf("kubeVersion %q: %w", cfg.KubeVersion, err)
		}
		install.KubeVersion = parsed
	}
	if len(cfg.APIVersions) > 0 {
		install.APIVersions = cfg.APIVersions
	}
	return nil
}

// manifestOf pulls the rendered YAML out of whatever the install returned.
//
// Helm 4 returns an interface rather than a concrete release, so this asserts
// to the version it knows and reports nothing rather than panicking if a future
// version returns something else.
func manifestOf(result any) string {
	if r, ok := result.(*release.Release); ok {
		return r.Manifest
	}
	return ""
}

// writeRendered is the shared tail of both rendering steps: write only when the
// content changed, so a re-run of a crossing leaves the tree clean (D23).
func writeRendered(out, name string, rendered []byte, step, action string) (passage.StepResult, error) {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: creating %s: %s", step, filepath.Dir(name), err)
	}
	if existing, err := os.ReadFile(out); err == nil && string(existing) == string(rendered) {
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("%s already holds this render", name),
			Output:  map[string]any{"changed": false, "file": name},
		}, nil
	}
	if err := os.WriteFile(out, rendered, 0o644); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: writing %s: %s", step, name, err)
	}
	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("%s into %s", action, name),
		Output:  map[string]any{"changed": true, "file": name},
	}, nil
}

func tidyErr(err error, workDir string) string {
	return strings.ReplaceAll(err.Error(), workDir+"/", "")
}

func (c RenderHelmConfig) check() error {
	switch {
	case c.Chart == "":
		return fmt.Errorf("chart is required")
	case c.Out == "":
		return fmt.Errorf("out is required — rendering writes a file for git-commit to pick up")
	case c.ReleaseName == "":
		// Not defaulted: templates commonly build resource names from it, so
		// guessing would silently name everything after something arbitrary.
		return fmt.Errorf("releaseName is required — chart templates name resources after it")
	}
	return nil
}

// CheckConfig implements passage.ConfigChecker.
func (r *RenderHelm) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[RenderHelmConfig](raw)
	if err != nil {
		return err
	}
	if err := cfg.check(); err != nil {
		return err
	}
	if cfg.KubeVersion != "" {
		if _, err := common.ParseKubeVersion(cfg.KubeVersion); err != nil {
			return fmt.Errorf("kubeVersion %q: %w", cfg.KubeVersion, err)
		}
	}
	return nil
}
