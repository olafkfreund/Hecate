package steps

import (
	"github.com/olafkfreund/hecate/pkg/health"
)

// ConfigType implementations, in one file rather than beside each step, for the
// same reason CheckConfig implementations are: the value is in their being
// uniform. Scattered across twelve files a missing one is invisible; in a
// single list it is a gap you can see, and TestEveryStepDescribesItsConfig
// enforces that there are none.
//
// Each returns the zero value of the struct the step's `with:` block decodes
// into — the same struct CheckConfig validates, so a generated schema and the
// admission check can never describe different things.

func (g *GitClone) ConfigType() any        { return GitCloneConfig{} }
func (g *GitCommit) ConfigType() any       { return GitCommitConfig{} }
func (g *GitPush) ConfigType() any         { return GitPushConfig{} }
func (g *GitPullRequest) ConfigType() any  { return GitPullRequestConfig{} }
func (e *EditYAML) ConfigType() any        { return EditYAMLConfig{} }
func (s *SetImage) ConfigType() any        { return SetImageConfig{} }
func (r *RenderKustomize) ConfigType() any { return RenderKustomizeConfig{} }
func (r *RenderHelm) ConfigType() any      { return RenderHelmConfig{} }
func (o *OCIPush) ConfigType() any         { return OCIPushConfig{} }
func (o *OCIPull) ConfigType() any         { return OCIPullConfig{} }
func (h *HTTP) ConfigType() any            { return HTTPConfig{} }
func (e *EvidenceGate) ConfigType() any    { return EvidenceGateConfig{} }
func (s *CommitStatus) ConfigType() any    { return CommitStatusConfig{} }
func (f *FluxReconcile) ConfigType() any   { return FluxReconcileConfig{} }

// FluxWait is the one step whose config is not declared beside it: it waits on
// exactly what a Gate's health check describes, and reuses that type rather
// than restating it. A second struct here would be two descriptions of one
// thing, free to disagree.
func (f *FluxWait) ConfigType() any { return health.FluxConfig{} }
