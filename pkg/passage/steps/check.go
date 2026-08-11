package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/olafkfreund/hecate/pkg/health"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// CheckConfig implementations, in one file rather than beside each step.
//
// They are here together because the value is in their being *uniform*: a Gate
// author should not have to know which steps happen to be checked. Scattered
// across ten files, a missing one is invisible; in a single list, it is a gap
// you can see. TestEveryStepChecksItsConfig enforces that there are no gaps.
//
// Each one decodes strictly — which alone catches the misspelt field #97 is
// about — and then repeats only those required-field checks that can be made
// without a cluster, a checkout or a Bundle. Anything needing those still
// reports at execution, because a check that guesses is worse than one that
// waits.

func (g *GitClone) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[GitCloneConfig](raw)
	if err != nil {
		return err
	}
	if cfg.Repo == "" {
		return fmt.Errorf("repo is required")
	}
	return nil
}

func (g *GitCommit) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[GitCommitConfig](raw)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Message) == "" {
		return fmt.Errorf("message is required")
	}
	return nil
}

func (g *GitPush) CheckConfig(raw json.RawMessage) error {
	_, err := passage.CheckConfig[GitPushConfig](raw)
	return err
}

func (g *GitPullRequest) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[GitPullRequestConfig](raw)
	if err != nil {
		return err
	}
	if cfg.CredentialsRef == nil {
		return fmt.Errorf("credentialsRef is required — opening a pull request needs an API token")
	}
	if cfg.Provider != "" && cfg.Provider != "github" && cfg.Provider != "gitlab" {
		return fmt.Errorf("provider %q is not one of github, gitlab", cfg.Provider)
	}
	return nil
}

func (e *EditYAML) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[EditYAMLConfig](raw)
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	if len(cfg.Edits) == 0 {
		return fmt.Errorf("no edits — a step that changes nothing is a mistake, not a no-op")
	}
	for i, e := range cfg.Edits {
		if e.Key == "" {
			return fmt.Errorf("edits[%d]: key is required", i)
		}
	}
	return nil
}

func (s *SetImage) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[SetImageConfig](raw)
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	if cfg.Image == "" {
		return fmt.Errorf("image is required")
	}
	return nil
}

func (f *FluxWait) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[health.FluxConfig](raw)
	if err != nil {
		return err
	}
	return checkFluxResources(cfg.Resources)
}

func (f *FluxReconcile) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[FluxReconcileConfig](raw)
	if err != nil {
		return err
	}
	return checkFluxResources(cfg.Resources)
}

// checkFluxResources judges what can be judged without a cluster: that
// resources were named at all, and that each kind is one Flux serves.
//
// Whether the resource exists is deliberately not checked — a Gate may
// legitimately be applied before the Kustomization it will wait for.
func checkFluxResources(resources []health.FluxResource) error {
	if len(resources) == 0 {
		return fmt.Errorf("at least one resource is required")
	}
	for i, r := range resources {
		if r.Name == "" {
			return fmt.Errorf("resources[%d]: name is required", i)
		}
		if _, err := r.GVK(); err != nil {
			return fmt.Errorf("resources[%d]: %w", i, err)
		}
	}
	return nil
}

func (h *HTTP) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[HTTPConfig](raw)
	if err != nil {
		return err
	}
	if cfg.URL == "" {
		return fmt.Errorf("url is required")
	}
	// Only when it is not an expression: `${{ vars.endpoint }}` is a URL we
	// cannot see yet, and refusing it would ban the interpolation the engine
	// exists to provide.
	if !strings.Contains(cfg.URL, "${{") &&
		!strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return fmt.Errorf("url %q must be http or https", cfg.URL)
	}
	for i, sh := range cfg.SecretHeaders {
		if sh.Name == "" {
			return fmt.Errorf("secretHeaders[%d]: name is required", i)
		}
		if sh.SecretRef == nil {
			return fmt.Errorf("secretHeaders[%d]: secretRef is required", i)
		}
	}
	return nil
}

func (e *EvidenceGate) CheckConfig(raw json.RawMessage) error {
	cfg, err := passage.CheckConfig[EvidenceGateConfig](raw)
	if err != nil {
		return err
	}
	if len(cfg.Gates) == 0 {
		return fmt.Errorf("no gates selected — a compliance step that checks nothing " +
			"is worse than no step, because it reads as a passed check")
	}
	for _, g := range cfg.Gates {
		switch g {
		case GateAssert, GateAllowlist, GatePolicy, GateChange:
		default:
			return fmt.Errorf("no gate named %q (have %s, %s, %s, %s)",
				g, GateAssert, GateAllowlist, GatePolicy, GateChange)
		}
	}
	if cfg.MaxRisk != nil && (*cfg.MaxRisk < 0 || *cfg.MaxRisk > 100) {
		return fmt.Errorf("maxRisk %d is outside 0-100", *cfg.MaxRisk)
	}
	return nil
}
