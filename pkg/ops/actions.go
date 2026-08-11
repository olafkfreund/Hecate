package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/gate"
)

// RefusedError is an action the rules do not allow.
//
// Separate from a failure to perform it: "this Bundle has not cleared staging"
// is an answer, not an error, and every surface presents the two differently.
type RefusedError struct {
	Action string
	Reason string
}

func (e *RefusedError) Error() string { return e.Action + " refused: " + e.Reason }

// IsRefused reports whether err is a RefusedError.
func IsRefused(err error) bool {
	var refused *RefusedError
	return errors.As(err, &refused)
}

// Promote asks a Gate to cross a Bundle, and returns the Passage it opened.
//
// The eligibility rules are the Gate controller's, not a second set: a crossing
// requested by hand is judged exactly as an automatic one would be. Skipping
// the check for manual requests is how "promote to production" becomes a way to
// bypass the pipeline.
func (o *Ops) Promote(ctx context.Context, namespace, gateName, bundleName, actor string) (*v1alpha1.Passage, error) {
	if actor == "" {
		// Recorded on the Passage and reported to the compliance system as who
		// asked. An anonymous promotion is worth less than no record at all,
		// because it looks like one.
		return nil, &RefusedError{Action: "promote", Reason: "no actor — a crossing must record who asked for it"}
	}

	g, err := o.Gate(ctx, namespace, gateName)
	if err != nil {
		return nil, err
	}
	if g.Spec.Suspend {
		return nil, &RefusedError{Action: "promote", Reason: fmt.Sprintf("Gate %s is suspended", gateName)}
	}
	if g.Spec.Passage == nil || len(g.Spec.Passage.Steps) == 0 {
		return nil, &RefusedError{Action: "promote", Reason: fmt.Sprintf("Gate %s has no steps", gateName)}
	}

	b, err := o.Bundle(ctx, namespace, bundleName)
	if err != nil {
		return nil, err
	}

	candidates := gate.Evaluate(g, []v1alpha1.Bundle{*b})
	if len(candidates) == 0 {
		return nil, &RefusedError{Action: "promote",
			Reason: fmt.Sprintf("Gate %s does not admit Bundles from Beacon %s", gateName, b.Spec.Beacon)}
	}
	if !candidates[0].Eligible {
		return nil, &RefusedError{Action: "promote", Reason: candidates[0].Reason}
	}

	// One crossing at a time. Two Passages writing the same environment would
	// race in git, and the loser's commit would be silently reverted.
	if active, err := o.activePassage(ctx, g); err != nil {
		return nil, err
	} else if active != nil {
		return nil, &RefusedError{Action: "promote",
			Reason: fmt.Sprintf("Passage %s is already crossing %s", active.Name, active.Spec.Bundle)}
	}

	if open, why := gate.Allowed(g.Spec.Windows, o.now().Time); !open {
		return nil, &RefusedError{Action: "promote", Reason: why}
	}

	// The controller's own construction, so a crossing asked for by a human is
	// identical to one it starts by itself — same labels, same copied steps.
	p := gate.NewPassage(g, b, actor)
	if err := o.Client.Create(ctx, p); err != nil {
		return nil, fmt.Errorf("creating Passage: %w", err)
	}
	return p, nil
}

// Approve records that a human has approved a Bundle for a Gate.
//
// Approval is per Gate, not per Bundle: approving something for staging must
// not approve it for production, which is the whole point of asking.
func (o *Ops) Approve(ctx context.Context, namespace, bundleName, gateName, actor string) error {
	if actor == "" {
		return &RefusedError{Action: "approve", Reason: "no actor — an approval must record who gave it"}
	}

	// The Gate must exist: approving for a name nobody has defined records an
	// approval that will never be read, and reads as done.
	if _, err := o.Gate(ctx, namespace, gateName); err != nil {
		return err
	}

	b, err := o.Bundle(ctx, namespace, bundleName)
	if err != nil {
		return err
	}
	if b.IsApprovedFor(gateName) {
		return nil // Already approved. Asking twice is not an error.
	}

	b.Status.ApprovedFor = append(b.Status.ApprovedFor, gateName)
	if err := o.Client.Status().Update(ctx, b); err != nil {
		return fmt.Errorf("recording approval: %w", err)
	}
	return nil
}

// Abort asks a running Passage to stop.
//
// It sets spec.abort rather than deleting: the Passage is the record of what
// happened, and deleting it would erase the evidence that a crossing was
// started and stopped (D18). The controller marks the remaining steps aborted.
func (o *Ops) Abort(ctx context.Context, namespace, passageName, actor string) error {
	if actor == "" {
		return &RefusedError{Action: "abort", Reason: "no actor — an abort must record who asked for it"}
	}

	p, err := o.Passage(ctx, namespace, passageName)
	if err != nil {
		return err
	}
	if p.Status.Phase.Terminal() {
		return &RefusedError{Action: "abort",
			Reason: fmt.Sprintf("Passage %s already finished (%s)", passageName, p.Status.Phase)}
	}
	if p.Spec.Abort {
		return nil // Already asked. Asking again is not an error.
	}

	p.Spec.Abort = true
	if err := o.Client.Update(ctx, p); err != nil {
		return fmt.Errorf("aborting Passage: %w", err)
	}
	return nil
}
