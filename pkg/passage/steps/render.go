package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepRenderKustomize is the value used in `steps[].uses`.
const StepRenderKustomize = "render-kustomize"

// Failure reasons for the rendering steps.
const (
	// ReasonRenderFailed means the sources are there but do not build.
	ReasonRenderFailed = "RenderFailed"
	// ReasonNothingRendered means the build produced no resources, which is
	// almost always a wrong path rather than an empty intention.
	ReasonNothingRendered = "NothingRendered"
)

// RenderKustomizeConfig is the `with:` block of a render-kustomize step.
type RenderKustomizeConfig struct {
	// Path is the kustomization directory, relative to the Passage work dir.
	Path string `json:"path"`
	// Out is where to write the rendered YAML, relative to the work dir. It is
	// a file, not a directory: the whole build is one document stream, and
	// splitting it per resource would make the git diff a rename storm the
	// first time anything is reordered.
	Out string `json:"out"`
	// LoadRestrictionsNone allows the kustomization to reference files outside
	// its own directory.
	//
	// Off by default, matching kustomize's own default and Flux's: a build that
	// can read anywhere in the checkout can read a file the author did not mean
	// to publish.
	LoadRestrictionsNone bool `json:"loadRestrictionsNone,omitempty"`
}

// RenderKustomize builds a kustomization and writes the result into the
// checkout, for the rendered-manifests pattern: what lands in git is final
// state, and Flux applies it without rendering anything itself.
//
// That is the point of rendering here rather than letting Flux do it. A repo
// holding rendered output can be diffed — a reviewer sees the manifests that
// will exist, not a kustomization whose effect they have to imagine — and the
// rendezvous stays a plain data format (D3).
type RenderKustomize struct{}

// NewRenderKustomize returns a render-kustomize step.
func NewRenderKustomize() *RenderKustomize { return &RenderKustomize{} }

// Name implements passage.Runner.
func (r *RenderKustomize) Name() string { return StepRenderKustomize }

// Run implements passage.Runner.
func (r *RenderKustomize) Run(_ context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[RenderKustomizeConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderKustomize, err)
	}
	if cfg.Path == "" || cfg.Out == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: path and out are both required", StepRenderKustomize)
	}

	dir, err := checkoutPath(sc.WorkDir, cfg.Path)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderKustomize, err)
	}
	out, err := checkoutPath(sc.WorkDir, cfg.Out)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepRenderKustomize, err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return passage.StepResult{}, passage.FailTerminal(ReasonFileNotFound,
			"%s: no directory at %s — check the path, and that a git-clone step ran first",
			StepRenderKustomize, cfg.Path)
	}

	options := krusty.MakeDefaultOptions()
	if cfg.LoadRestrictionsNone {
		options.LoadRestrictions = types.LoadRestrictionsNone
	}

	// The real filesystem rather than an in-memory copy: a kustomization can
	// reference generator files by path, and loading the tree into memory first
	// would mean reimplementing kustomize's own resolution.
	built, err := krusty.MakeKustomizer(options).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		// A kustomization that does not build will not build on the next
		// attempt either.
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: %s", StepRenderKustomize, tidyKustomizeError(err, sc.WorkDir))
	}

	// AsYaml sorts by the kustomize ordering, which is stable for a given
	// input. That matters more here than anywhere else: this output is
	// committed, and a render that reshuffled on every run would produce a diff
	// on every crossing and teach reviewers to skim them.
	rendered, err := built.AsYaml()
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: serialising the build: %s", StepRenderKustomize, err)
	}
	if len(strings.TrimSpace(string(rendered))) == 0 {
		return passage.StepResult{}, passage.FailTerminal(ReasonNothingRendered,
			"%s: %s built no resources — check the kustomization's resources list",
			StepRenderKustomize, cfg.Path)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: creating %s: %s", StepRenderKustomize, filepath.Dir(cfg.Out), err)
	}

	// Unchanged output is not rewritten, so a re-run of a crossing leaves the
	// tree clean and git-commit reports nothing to do (D23).
	existing, readErr := os.ReadFile(out)
	if readErr == nil && string(existing) == string(rendered) {
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("%s already holds this build (%s)", cfg.Out, plural(built.Size(), "resource")),
			Output:  map[string]any{"changed": false, "file": cfg.Out, "resources": built.Size()},
		}, nil
	}

	if err := os.WriteFile(out, rendered, 0o644); err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonRenderFailed,
			"%s: writing %s: %s", StepRenderKustomize, cfg.Out, err)
	}
	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("rendered %s into %s (%s)", cfg.Path, cfg.Out, plural(built.Size(), "resource")),
		Output:  map[string]any{"changed": true, "file": cfg.Out, "resources": built.Size()},
	}, nil
}

// tidyKustomizeError strips the work directory from kustomize's messages.
//
// They quote absolute paths, and the work dir is a scratch directory named
// after a Passage UID — showing it tells the reader nothing and hides the
// repository-relative path they actually recognise.
func tidyKustomizeError(err error, workDir string) string {
	return strings.ReplaceAll(err.Error(), workDir+"/", "")
}

// CheckConfig implements passage.ConfigChecker.
func (r *RenderKustomize) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[RenderKustomizeConfig](raw)
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	if cfg.Out == "" {
		return fmt.Errorf("out is required — rendering writes a file for git-commit to pick up")
	}
	return nil
}
