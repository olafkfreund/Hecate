package ops

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/gate"
)

// State is the one-word answer to "what is this Gate doing?".
type State string

const (
	// StateCrossing means a Passage is running now.
	StateCrossing State = "Crossing"
	// StateReady means a Bundle could cross and is waiting to be asked.
	StateReady State = "Ready"
	// StateBlocked means something must change before anything can cross.
	StateBlocked State = "Blocked"
	// StateIdle means there is nothing to do: nothing new has appeared.
	StateIdle State = "Idle"
	// StateFailed means the last crossing failed and has not been retried.
	StateFailed State = "Failed"
)

// Explanation is why a Gate is where it is.
//
// The question "why is nothing crossing?" is currently answered by reading four
// resources by hand and knowing which fields matter. This answers it once, in
// structure a CLI can print, an API can serialise and a model can reason over —
// which is why the reasons are a list of typed causes rather than a sentence.
type Explanation struct {
	Gate      string `json:"gate"`
	Namespace string `json:"namespace"`
	State     State  `json:"state"`
	// Summary is one line, suitable on its own.
	Summary string `json:"summary"`
	// Blockers are the specific things standing in the way, most actionable
	// first. Empty when nothing is wrong.
	Blockers []Blocker `json:"blockers,omitempty"`
	// Current is the Bundle in the environment now, if any.
	Current string `json:"current,omitempty"`
	// Eligible names Bundles that could cross right now.
	Eligible []string `json:"eligible,omitempty"`
	// Waiting names Bundles that cannot, with the reason for each.
	Waiting []Waiting `json:"waiting,omitempty"`
	// Health is the Gate's own assessment of what it watches.
	Health v1alpha1.Health `json:"health,omitempty"`
	// Evidence is the change gate's verdict for the crossing in progress, when
	// there is one. Carried whole so a caller can show the risk score next to
	// the reasons rather than parsing them back out of a sentence.
	Evidence *v1alpha1.EvidenceRef `json:"evidence,omitempty"`
}

// Blocker is one reason nothing is crossing.
type Blocker struct {
	// Kind is a stable code, so a caller can branch without parsing prose.
	Kind BlockerKind `json:"kind"`
	// Detail is the human-readable specifics.
	Detail string `json:"detail"`
	// Fix is what would unblock it, when there is a single obvious answer.
	Fix string `json:"fix,omitempty"`
}

// BlockerKind enumerates why a Gate is not crossing. Stable strings: a UI
// chooses an icon from these and an LLM reasons over them.
type BlockerKind string

const (
	BlockerSuspended     BlockerKind = "Suspended"
	BlockerInvalidSteps  BlockerKind = "InvalidSteps"
	BlockerNoPassage     BlockerKind = "NoPassageTemplate"
	BlockerNoBundles     BlockerKind = "NoBundles"
	BlockerNotApproved   BlockerKind = "AwaitingApproval"
	BlockerUpstream      BlockerKind = "UpstreamNotCleared"
	BlockerWindowClosed  BlockerKind = "WindowClosed"
	BlockerPassageFailed BlockerKind = "PassageFailed"
	BlockerStepWaiting   BlockerKind = "StepWaiting"
	BlockerChangeHeld    BlockerKind = "ChangeHeld"
	BlockerUnhealthy     BlockerKind = "Unhealthy"
	BlockerManual        BlockerKind = "AwaitingRequest"
)

// Waiting is one Bundle that cannot cross, and why.
type Waiting struct {
	Bundle string `json:"bundle"`
	Reason string `json:"reason"`
	// Kind is the same reason as a stable code, matching BlockerKind's values
	// where they overlap. Without it the approval queue would have to decide
	// what is waiting on a human by matching the prose, and rewording a message
	// would break a caller.
	Kind gate.Code `json:"kind,omitempty"`
}

// Explain answers "why is this Gate not crossing anything?".
//
// It composes the rules rather than restating them: eligibility comes from
// pkg/gate's own judgement, and the window from its own check. A second
// implementation of either would be a second answer to the same question, which
// is the failure this package exists to prevent.
func (o *Ops) Explain(ctx context.Context, namespace, name string) (*Explanation, error) {
	g, err := o.Gate(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	ex := &Explanation{Gate: name, Namespace: namespace}
	if g.Status.Current != nil {
		ex.Current = g.Status.Current.Bundle
	}
	if g.Status.Health != nil {
		ex.Health = g.Status.Health.Status
	}

	// Suspended is the whole answer: nothing else about the Gate matters while
	// it is off, and listing further blockers would imply they need fixing.
	if g.Spec.Suspend {
		ex.State = StateBlocked
		ex.Summary = "the Gate is suspended"
		ex.Blockers = []Blocker{{
			Kind: BlockerSuspended, Detail: "spec.suspend is true",
			Fix: fmt.Sprintf("kubectl -n %s patch gate %s --type=merge -p '{\"spec\":{\"suspend\":false}}'", namespace, name),
		}}
		return ex, nil
	}

	// Likewise a Gate whose steps will not run: it opens no Passage at all, so
	// everything downstream of that is moot.
	if cond := meta.FindStatusCondition(g.Status.Conditions, v1alpha1.ConditionReady); cond != nil &&
		cond.Status == metav1.ConditionFalse && cond.Reason == gate.ReasonInvalidSteps {
		ex.State = StateBlocked
		ex.Summary = "the Gate's steps are invalid, so it will not open a Passage"
		ex.Blockers = []Blocker{{
			Kind: BlockerInvalidSteps, Detail: cond.Message,
			Fix: "correct spec.passage.steps and re-apply",
		}}
		return ex, nil
	}

	if g.Spec.Passage == nil || len(g.Spec.Passage.Steps) == 0 {
		ex.State = StateBlocked
		ex.Summary = "the Gate has no steps, so a crossing would do nothing"
		ex.Blockers = []Blocker{{
			Kind: BlockerNoPassage, Detail: "spec.passage.steps is empty",
			Fix: "add the steps that move a Bundle into this environment",
		}}
		return ex, nil
	}

	// An in-flight Passage answers the question by itself: whatever else is
	// true, this is what the Gate is doing.
	if active, err := o.activePassage(ctx, g); err != nil {
		return nil, err
	} else if active != nil {
		return o.explainActive(ex, active), nil
	}

	// Nothing running. Judge what could cross, using the Gate controller's own
	// rules.
	bundles, err := o.Bundles(ctx, namespace)
	if err != nil {
		return nil, err
	}
	candidates := gate.Evaluate(g, bundles)
	for _, c := range candidates {
		if c.Eligible {
			ex.Eligible = append(ex.Eligible, c.Bundle.Name)
			continue
		}
		// "already in this Gate" is not a blocker — it is the goal.
		if c.Reason == "already in this Gate" {
			continue
		}
		ex.Waiting = append(ex.Waiting, Waiting{
			Bundle: c.Bundle.Name, Reason: c.Reason, Kind: c.Code,
		})
	}

	ex.Blockers = append(ex.Blockers, o.lastFailure(ctx, g)...)
	ex.Blockers = append(ex.Blockers, healthBlocker(g)...)
	o.judge(ex, g, candidates)
	headlineFailure(ex)
	return ex, nil
}

// judge sets the state and summary from what the candidates showed.
func (o *Ops) judge(ex *Explanation, g *v1alpha1.Gate, candidates []gate.Candidate) {
	switch {
	case len(ex.Eligible) > 0:
		// Something could cross. Either a window is holding it, or the Gate is
		// manual and nobody has asked.
		if open, why := gate.Allowed(g.Spec.Windows, o.now().Time); !open {
			ex.State = StateBlocked
			ex.Summary = fmt.Sprintf("%s could cross, but %s", plural(len(ex.Eligible), "Bundle"), why)
			ex.Blockers = append(ex.Blockers, Blocker{
				Kind: BlockerWindowClosed, Detail: why,
				Fix: "wait for the window, or adjust spec.windows",
			})
			return
		}
		if !g.Spec.Auto {
			ex.State = StateReady
			ex.Summary = fmt.Sprintf("%s ready to cross; this Gate does not cross automatically",
				plural(len(ex.Eligible), "Bundle"))
			ex.Blockers = append(ex.Blockers, Blocker{
				Kind: BlockerManual, Detail: "spec.auto is false, so a crossing must be requested",
				Fix: fmt.Sprintf("hecate promote %s --bundle %s", ex.Gate, ex.Eligible[0]),
			})
			return
		}
		// Auto, open window, eligible — the controller is about to act.
		ex.State = StateReady
		ex.Summary = fmt.Sprintf("%s eligible; the Gate crosses automatically and has not yet",
			plural(len(ex.Eligible), "Bundle"))

	case len(ex.Waiting) > 0:
		ex.State = StateBlocked
		ex.Summary = fmt.Sprintf("%s cannot cross: %s",
			plural(len(ex.Waiting), "Bundle"), ex.Waiting[0].Reason)
		ex.Blockers = append(ex.Blockers, waitingBlockers(ex.Waiting)...)

	case len(candidates) == 0:
		ex.State = StateIdle
		ex.Summary = "no Bundle from any admitted Beacon"
		ex.Blockers = append(ex.Blockers, Blocker{
			Kind:   BlockerNoBundles,
			Detail: "no Beacon this Gate admits has produced a Bundle",
			Fix:    "check the Beacon is discovering artifacts: kubectl get beacons",
		})

	default:
		ex.State = StateIdle
		ex.Summary = "nothing to do — everything admitted is already in this Gate"
	}
}

// headlineFailure promotes a failed last crossing to the summary.
//
// Applied after judging rather than inside it: judge returns early on several
// paths, and a check placed at its end silently did not run for them — a Gate
// with an eligible Bundle reported "ready to cross" while its last crossing had
// failed on a rejected credential.
func headlineFailure(ex *Explanation) {
	for _, b := range ex.Blockers {
		if b.Kind != BlockerPassageFailed {
			continue
		}
		ex.State = StateFailed
		ex.Summary = b.Detail
		return
	}
}

// waitingBlockers turns per-Bundle reasons into typed causes, collapsing the
// repeats: ten Bundles awaiting the same approval is one thing to fix.
func waitingBlockers(waiting []Waiting) []Blocker {
	seen := map[BlockerKind]bool{}
	var out []Blocker
	for _, w := range waiting {
		var b Blocker
		switch {
		case strings.HasPrefix(w.Reason, "awaiting approval"):
			b = Blocker{Kind: BlockerNotApproved, Detail: w.Reason,
				Fix: fmt.Sprintf("hecate approve %s", w.Bundle)}
		case strings.HasPrefix(w.Reason, "has not cleared"):
			b = Blocker{Kind: BlockerUpstream, Detail: w.Reason,
				Fix: "promote it through the upstream Gate first"}
		default:
			continue
		}
		if seen[b.Kind] {
			continue
		}
		seen[b.Kind] = true
		out = append(out, b)
	}
	return out
}

// explainActive describes a Passage that is running now.
func (o *Ops) explainActive(ex *Explanation, p *v1alpha1.Passage) *Explanation {
	ex.State = StateCrossing
	ex.Summary = fmt.Sprintf("crossing %s", p.Spec.Bundle)
	ex.Evidence = p.Status.Evidence

	// A change gate holding is a distinct answer from a step being slow: the
	// crossing is working exactly as designed and is waiting on a person, so it
	// gets its own code and names what would release it. Reported ahead of the
	// generic step blocker, because "waiting on evidence-gate: change gate is
	// holding" says less than the verdict does.
	if ev := p.Status.Evidence; ev != nil && ev.Verdict != "" && ev.Verdict != "approve" {
		detail := fmt.Sprintf("the change gate returned %s", ev.Verdict)
		if ev.Risk != nil {
			detail = fmt.Sprintf("%s (risk %d/100)", detail, *ev.Risk)
		}
		if len(ev.Blockers) > 0 {
			detail = fmt.Sprintf("%s: %s", detail, strings.Join(ev.Blockers, "; "))
		}
		ex.Summary = fmt.Sprintf("crossing %s — held by the change gate", p.Spec.Bundle)
		ex.Blockers = append(ex.Blockers, Blocker{
			Kind:   BlockerChangeHeld,
			Detail: detail,
			Fix: "resolve what the change gate is asking for in Fides — the crossing " +
				"re-reads the verdict and continues on its own",
		})
		return ex
	}

	// The step that is running is what the Gate is actually waiting for, and
	// its message is what the step itself chose to say — a held change gate, a
	// pull request awaiting review, a Flux resource not yet converged.
	for i := range p.Status.Steps {
		st := &p.Status.Steps[i]
		if st.Phase != v1alpha1.StepRunning {
			continue
		}
		detail := st.Uses
		if st.Message != "" {
			detail = fmt.Sprintf("%s: %s", st.Uses, st.Message)
		}
		ex.Summary = fmt.Sprintf("crossing %s — waiting on %s", p.Spec.Bundle, detail)
		ex.Blockers = append(ex.Blockers, Blocker{Kind: BlockerStepWaiting, Detail: detail})
		break
	}
	return ex
}

// activePassage finds the Gate's in-flight Passage, if any.
//
// The Gate's own status names it, but a Passage that started since the last
// reconcile would be missed, so the list is the authority and status is only a
// shortcut.
func (o *Ops) activePassage(ctx context.Context, g *v1alpha1.Gate) (*v1alpha1.Passage, error) {
	passages, err := o.Passages(ctx, g.Namespace, g.Name, "")
	if err != nil {
		return nil, err
	}
	for i := range passages {
		if !passages[i].Status.Phase.Terminal() {
			return &passages[i], nil
		}
	}
	return nil, nil
}

// lastFailure reports the most recent failed crossing, when it is the latest
// thing that happened.
//
// Only the latest: a Passage that failed before a later one succeeded is
// history, and reporting it as a blocker would make a working Gate look broken.
func (o *Ops) lastFailure(ctx context.Context, g *v1alpha1.Gate) []Blocker {
	passages, err := o.Passages(ctx, g.Namespace, g.Name, "")
	if err != nil || len(passages) == 0 {
		return nil
	}
	last := passages[0]
	if last.Status.Phase != v1alpha1.PassageFailed {
		return nil
	}

	detail := fmt.Sprintf("the last crossing of %s failed: %s", last.Spec.Bundle, last.Status.Message)
	for i := range last.Status.Steps {
		st := &last.Status.Steps[i]
		if st.Phase != v1alpha1.StepFailed {
			continue
		}
		// The reason code is what makes this classifiable rather than merely
		// readable (D21).
		detail = fmt.Sprintf("the last crossing of %s failed at %s", last.Spec.Bundle, st.Uses)
		if st.Reason != "" {
			detail += " [" + st.Reason + "]"
		}
		if st.Message != "" {
			detail += ": " + st.Message
		}
		break
	}
	return []Blocker{{
		Kind: BlockerPassageFailed, Detail: detail,
		Fix: fmt.Sprintf("kubectl -n %s describe passage %s", g.Namespace, last.Name),
	}}
}

// healthBlocker reports what the Gate watches being unhealthy.
//
// Not a reason nothing crosses — a Degraded Gate still admits Bundles — but it
// is nearly always the thing the operator actually wanted to know.
func healthBlocker(g *v1alpha1.Gate) []Blocker {
	if g.Status.Health == nil {
		return nil
	}
	switch g.Status.Health.Status {
	case v1alpha1.HealthDegraded, v1alpha1.HealthUnknown:
	default:
		return nil
	}
	detail := fmt.Sprintf("what this Gate watches is %s", g.Status.Health.Status)
	if len(g.Status.Health.Issues) > 0 {
		detail = fmt.Sprintf("%s: %s", detail, strings.Join(g.Status.Health.Issues, "; "))
	}
	return []Blocker{{Kind: BlockerUnhealthy, Detail: detail}}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
