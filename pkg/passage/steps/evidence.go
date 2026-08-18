package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// StepEvidenceGate is the value used in `steps[].uses`.
const StepEvidenceGate = "evidence-gate"

// The four gates, named as they are in `with.gates`.
const (
	// GateAssert checks the Bundle's artifacts against a Fides policy.
	GateAssert = "assert"
	// GateAllowlist checks the artifacts are approved for this environment.
	GateAllowlist = "allowlist"
	// GatePolicy checks the environment's policy against the build's trail.
	GatePolicy = "policy"
	// GateChange asks for the evidence-backed change-approval verdict.
	GateChange = "change"
)

// Failure reasons this step can report.
const (
	// ReasonEvidenceUnavailable means Fides could not be reached or refused the
	// token. Distinct from a refusal: one needs the compliance system back, the
	// other needs the change fixed.
	ReasonEvidenceUnavailable = "EvidenceUnavailable"
	// ReasonNotCompliant means an artifact failed a policy.
	ReasonNotCompliant = "NotCompliant"
	// ReasonNotAllowlisted means an artifact is not approved for the environment.
	ReasonNotAllowlisted = "NotAllowlisted"
	// ReasonNoEvidence means the artifact has no trail in Fides, so the
	// trail-scoped gates have nothing to judge.
	ReasonNoEvidence = "NoEvidence"
	// ReasonChangeHeld means the change gate withheld approval for longer than
	// holdTimeout allows.
	ReasonChangeHeld = "ChangeHeld"
)

const (
	defaultHoldPoll    = time.Minute
	defaultHoldTimeout = 24 * time.Hour
)

// EvidenceGateConfig is the `with:` block of an evidence-gate step.
type EvidenceGateConfig struct {
	// Gates selects which checks apply. Empty runs none, which is a mistake
	// worth reporting rather than a quiet pass.
	//
	// A dev Gate can run none of them and production all four.
	Gates []string `json:"gates"`
	// Policy is the Fides policy name for the `assert` gate. Empty asks Fides
	// for every policy that applies to the artifact.
	Policy string `json:"policy,omitempty"`
	// MaxRisk fails the crossing when the change gate's risk score exceeds it,
	// letting a team be stricter than Fides' own verdict. 0-100.
	MaxRisk *int32 `json:"maxRisk,omitempty"`
	// ReportArtifacts records every image digest in the Bundle against the
	// trail, so a change gate sees the whole release rather than the one
	// artifact whose trail was looked up.
	//
	// Off by default, and deliberately: Fides upserts on the digest and
	// overwrites the trail link, so this claims the other images belong to
	// *this* trail. That is true when one CI run built them all and wrong when
	// they came from different builds — which only the operator knows.
	ReportArtifacts bool `json:"reportArtifacts,omitempty"`
	// HoldTimeout bounds how long a held change may wait before the crossing
	// fails. Default 24h.
	HoldTimeout *metav1.Duration `json:"holdTimeout,omitempty"`
	// PollInterval is how often a held change is re-read. Default 1m.
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`
}

// EvidenceGate consults Fides before a crossing proceeds.
//
// The four gates answer different questions and fail differently. Three are
// verdicts about the change — is this artifact compliant, is it allowed here,
// does this environment's policy accept it — and a no from any of them is
// terminal, because the answer will not change until somebody changes something.
//
// The change gate is not like the others. A hold means a human has not signed
// off yet, which is the control working rather than the crossing being broken,
// so it reports Running and waits. It fails only once the wait exceeds
// holdTimeout, on the same reasoning as D6: waiting forever and failing
// immediately are both wrong.
type EvidenceGate struct {
	client client.Client
	// defaultServer is the controller's --fides-server, used when a Gate does
	// not name one, so a fleet with a single Fides says it once.
	defaultServer string
	// dial is injectable so tests can point the step at a fake Fides.
	dial func(fides.Config) (*fides.Client, error)
}

// NewEvidenceGate returns an evidence-gate step.
func NewEvidenceGate(c client.Client, defaultServer string) *EvidenceGate {
	return &EvidenceGate{client: c, defaultServer: defaultServer, dial: fides.New}
}

// Name implements passage.Runner.
func (e *EvidenceGate) Name() string { return StepEvidenceGate }

// Run implements passage.Runner.
func (e *EvidenceGate) Run(ctx context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[EvidenceGateConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepEvidenceGate, err)
	}
	if len(cfg.Gates) == 0 {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: no gates selected — a compliance step that checks nothing is worse than no step, "+
				"because it reads as a passed check", StepEvidenceGate)
	}
	for _, g := range cfg.Gates {
		switch g {
		case GateAssert, GateAllowlist, GatePolicy, GateChange:
		default:
			return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
				"%s: no gate named %q (have %s, %s, %s, %s)",
				StepEvidenceGate, g, GateAssert, GateAllowlist, GatePolicy, GateChange)
		}
	}

	evidence, err := e.evidenceConfig(ctx, sc)
	if err != nil {
		return passage.StepResult{}, err
	}
	fidesClient, err := e.connect(ctx, sc, evidence)
	if err != nil {
		return passage.StepResult{}, err
	}

	digests := passage.ImageDigests(sc.Bundle)
	if len(digests) == 0 {
		return passage.StepResult{}, passage.FailTerminal(ReasonNoEvidence,
			"%s: the Bundle pins no image digests — there is nothing to check compliance of. "+
				"A digest can be moved to point at other content; a tag cannot be gated on",
			StepEvidenceGate)
	}

	output := map[string]any{}
	selected := map[string]bool{}
	for _, g := range cfg.Gates {
		selected[g] = true
	}

	// Digest-scoped gates first: they need nothing but the Bundle, and failing
	// here saves a trail lookup.
	if selected[GateAssert] {
		if err := e.assert(ctx, fidesClient, cfg, digests); err != nil {
			return passage.StepResult{Output: output}, err
		}
	}
	if selected[GateAllowlist] {
		if err := e.allowlist(ctx, fidesClient, evidence.FidesEnvironment, digests); err != nil {
			return passage.StepResult{Output: output}, err
		}
	}

	if !selected[GatePolicy] && !selected[GateChange] {
		return passage.StepResult{
			Phase:   v1alpha1.StepSucceeded,
			Message: fmt.Sprintf("%s cleared", strings.Join(cfg.Gates, " and ")),
			Output:  output,
		}, nil
	}

	// The trail-scoped gates judge the trail CI built the artifact on, which is
	// where the SBOM and scan attestations live.
	trail, err := fidesClient.TrailForArtifact(ctx, digests[0])
	if err != nil {
		return passage.StepResult{Output: output}, unavailable("looking up the artifact's trail", err)
	}
	if trail == "" {
		return passage.StepResult{Output: output}, passage.FailTerminal(ReasonNoEvidence,
			"%s: Fides has no record of %s, so there is no evidence to judge — "+
				"the build that produced it did not report the artifact",
			StepEvidenceGate, digests[0])
	}
	output["trail"] = trail

	// After the trail is known, never before: reporting a digest with no trail
	// would overwrite the link CI made and detach the evidence being judged.
	if cfg.ReportArtifacts {
		reported, err := e.report(ctx, fidesClient, sc.Bundle, trail)
		if err != nil {
			return passage.StepResult{Output: output}, unavailable("reporting the Bundle's artifacts", err)
		}
		output["artifactsReported"] = reported
	}

	if selected[GatePolicy] {
		verdict, err := fidesClient.PolicyCheck(ctx, evidence.FidesEnvironment, trail)
		if err != nil {
			return passage.StepResult{Output: output}, unavailable("checking the environment policy", err)
		}
		if !verdict.Compliant {
			return passage.StepResult{Output: output}, passage.FailTerminal(ReasonNotCompliant,
				"%s: the environment policy is not satisfied: %s",
				StepEvidenceGate, strings.Join(verdict.Unmet(), "; "))
		}
	}

	if selected[GateChange] {
		// Before the verdict is read, never after: Fides evaluates segregation
		// of duties from the identities recorded on the trail, and a verdict
		// read first would be the one that says nobody is deploying this.
		if err := e.recordDeployer(ctx, fidesClient, sc, trail); err != nil {
			return passage.StepResult{Output: output}, err
		}
		return e.changeGate(ctx, fidesClient, sc, cfg, trail, output)
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: fmt.Sprintf("%s cleared", strings.Join(cfg.Gates, " and ")),
		Output:  output,
		Evidence: &v1alpha1.EvidenceRef{
			Trail: trail,
		},
	}, nil
}

// assert checks every artifact against the Fides policy.
func (e *EvidenceGate) assert(
	ctx context.Context, c *fides.Client, cfg EvidenceGateConfig, digests []string,
) error {
	for _, digest := range digests {
		out, err := c.Assert(ctx, digest, cfg.Policy)
		if err != nil {
			return unavailable("asserting policy compliance", err)
		}
		if !out.Compliant {
			return passage.FailTerminal(ReasonNotCompliant,
				"%s: %s is not compliant: %s",
				StepEvidenceGate, digest, strings.Join(out.Violations, "; "))
		}
	}
	return nil
}

// allowlist checks every artifact is approved for the environment.
func (e *EvidenceGate) allowlist(
	ctx context.Context, c *fides.Client, environment string, digests []string,
) error {
	for _, digest := range digests {
		approved, err := c.Allowlisted(ctx, environment, digest)
		if err != nil {
			return unavailable("checking the environment allowlist", err)
		}
		if !approved {
			// --reason is not optional decoration. Fides requires it as of
			// v0.7.0: an allowlist entry is an accepted risk, and one with no
			// stated justification cannot be evaluated by an auditor later.
			// Without the flag this command now fails, so a hint that omits it
			// sends the operator to a dead end at exactly the moment they are
			// already blocked.
			return passage.FailTerminal(ReasonNotAllowlisted,
				"%s: %s is not on this environment's allowlist — approve it with "+
					"`fides allowlist add --env %s --sha %s --reason \"<why this is accepted>\"`",
				StepEvidenceGate, digest, environment, strings.TrimPrefix(digest, "sha256:"))
		}
	}
	return nil
}

// recordDeployer tells Fides who is performing this deployment, which is the
// third identity its segregation-of-duties check needs — the other two being
// the committer CI recorded and the approver `hecate approve` recorded.
//
// **An automatic crossing records nobody.** Hecate is not a person, and naming
// it as the deployer would let a change pass four-eyes with two humans and a
// robot. The change gate then holds with "no deployer recorded", which is the
// correct answer: a control requiring a human to deploy is not satisfied by
// nothing having required one.
func (e *EvidenceGate) recordDeployer(
	ctx context.Context, c *fides.Client, sc *passage.StepContext, trail string,
) error {
	if sc.Actor == "" || sc.Actor == passage.ActorController {
		return nil
	}
	err := c.RecordApproval(ctx, trail, fides.Approval{
		By:     sc.Actor,
		Role:   fides.RoleDeployer,
		Reason: fmt.Sprintf("crossing Gate %s in %s", sc.Gate, sc.Namespace),
	})
	if err != nil {
		// An uncounted approval is a configuration problem, not an outage.
		// Retrying it would leave the crossing waiting on a signature that has
		// already been given, for ever, which is precisely the silence this
		// distinction exists to break (#132).
		if fides.IsUncounted(err) {
			return passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepEvidenceGate, err)
		}
		return unavailable("recording the deployer", err)
	}
	return nil
}

// changeGate reads the change-approval verdict, waiting while it is held.
func (e *EvidenceGate) changeGate(
	ctx context.Context, c *fides.Client, sc *passage.StepContext,
	cfg EvidenceGateConfig, trail string, output map[string]any,
) (passage.StepResult, error) {
	verdict, err := c.ChangeGate(ctx, trail)
	if err != nil {
		return passage.StepResult{Output: output}, unavailable("reading the change gate", err)
	}
	output["verdict"] = verdict.Recommendation
	output["risk"] = verdict.RiskScore

	risk := int32(verdict.RiskScore) //nolint:gosec // Fides bounds this to 0-100
	// Blockers travel with the verdict rather than only in this step's message:
	// the Gate mirrors this ref, and "hold, risk 62" with no reasons is a number
	// to escalate rather than a thing to fix.
	ref := &v1alpha1.EvidenceRef{
		Trail: trail, Verdict: verdict.Recommendation, Risk: &risk, Blockers: verdict.Blockers(),
	}

	// A team may be stricter than Fides' own verdict. This is terminal even
	// though the gate approved: waiting will not lower the score, because the
	// score is about the evidence that already exists.
	if cfg.MaxRisk != nil && risk > *cfg.MaxRisk {
		return passage.StepResult{Output: output, Evidence: ref}, passage.FailTerminal(ReasonNotCompliant,
			"%s: risk score %d exceeds the Gate's maxRisk of %d (%s)",
			StepEvidenceGate, verdict.RiskScore, *cfg.MaxRisk, verdict.Summary)
	}

	if !verdict.Held() {
		return passage.StepResult{
			Phase:    v1alpha1.StepSucceeded,
			Message:  fmt.Sprintf("change approved, risk %d (%s)", verdict.RiskScore, verdict.RiskLevel),
			Output:   output,
			Evidence: ref,
		}, nil
	}

	// Held. A human has not signed off, which is the control doing its job, so
	// this waits rather than fails — but not forever (D6).
	timeout := defaultHoldTimeout
	if cfg.HoldTimeout != nil && cfg.HoldTimeout.Duration > 0 {
		timeout = cfg.HoldTimeout.Duration
	}
	if waited := time.Since(sc.StartedAt); waited > timeout {
		return passage.StepResult{Output: output, Evidence: ref}, passage.FailTerminal(ReasonChangeHeld,
			"%s: the change gate has withheld approval for %s, longer than holdTimeout (%s): %s",
			StepEvidenceGate, waited.Round(time.Minute), timeout, strings.Join(verdict.Blockers(), "; "))
	}

	poll := defaultHoldPoll
	if cfg.PollInterval != nil && cfg.PollInterval.Duration > 0 {
		poll = cfg.PollInterval.Duration
	}
	return passage.StepResult{
		Phase: v1alpha1.StepRunning,
		Message: fmt.Sprintf("change gate is holding (risk %d): %s",
			verdict.RiskScore, strings.Join(verdict.Blockers(), "; ")),
		Output:     output,
		Evidence:   ref,
		RetryAfter: poll,
	}, nil
}

// evidenceConfig reads the Gate's compliance settings.
func (e *EvidenceGate) evidenceConfig(
	ctx context.Context, sc *passage.StepContext,
) (*v1alpha1.EvidenceConfig, error) {
	if e.client == nil {
		return nil, passage.FailTerminal(ReasonInvalidConfig, "%s: the step has no client", StepEvidenceGate)
	}
	var gate v1alpha1.Gate
	key := client.ObjectKey{Namespace: sc.Namespace, Name: sc.Gate}
	if err := e.client.Get(ctx, key, &gate); err != nil {
		return nil, passage.FailTerminal(ReasonInvalidConfig,
			"%s: reading Gate %s: %s", StepEvidenceGate, sc.Gate, err)
	}
	if gate.Spec.Evidence == nil || gate.Spec.Evidence.FidesEnvironment == "" {
		return nil, passage.FailTerminal(ReasonInvalidConfig,
			"%s: Gate %s has no evidence.fidesEnvironment, so there is no environment to check against",
			StepEvidenceGate, sc.Gate)
	}
	return gate.Spec.Evidence, nil
}

// connect builds a Fides client from the Gate's settings.
func (e *EvidenceGate) connect(
	ctx context.Context, sc *passage.StepContext, evidence *v1alpha1.EvidenceConfig,
) (*fides.Client, error) {
	c, err := passage.DialFides(ctx, e.client, sc.Namespace, e.defaultServer, evidence, e.dial)
	if err != nil {
		return nil, passage.FailTerminal(ReasonInvalidConfig,
			"%s: Gate %s: %s", StepEvidenceGate, sc.Gate, err)
	}
	return c, nil
}

// unavailable classifies a Fides failure. A rejected token will not start
// working; an outage might, and blocking every promotion permanently because
// the compliance system restarted would be its own kind of outage.
func unavailable(what string, err error) error {
	if fides.IsAuth(err) {
		return passage.FailTerminal(ReasonEvidenceUnavailable, "%s: %s: %s", StepEvidenceGate, what, err)
	}
	return passage.Fail(ReasonEvidenceUnavailable, "%s: %s: %s", StepEvidenceGate, what, err)
}

// report records the Bundle's images against the trail, so the change gate
// judges the release rather than one image of it.
//
// The first digest is skipped only in the sense that reporting it is harmless:
// it is the one the trail was looked up from, so the link already exists and
// the upsert is a no-op.
func (e *EvidenceGate) report(
	ctx context.Context, c *fides.Client, bundle *v1alpha1.Bundle, trail string,
) (int, error) {
	n := 0
	for _, a := range bundle.Spec.Artifacts {
		if a.Image == nil || a.Image.Digest == "" {
			continue
		}
		err := c.ReportArtifact(ctx, fides.Artifact{
			SHA256: a.Image.Digest,
			Trail:  trail,
			Name:   a.Image.Repo,
			Type:   "container-image",
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
