package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/gate"
	"github.com/olafkfreund/hecate/pkg/passage"
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
	g, err := o.Gate(ctx, namespace, gateName)
	if err != nil {
		return err
	}

	b, err := o.Bundle(ctx, namespace, bundleName)
	if err != nil {
		return err
	}
	if b.IsApprovedFor(gateName) {
		return nil // Already approved. Asking twice is not an error.
	}

	// Fides first, the cluster second, and the order is the point.
	//
	// If the cluster were written first, a retry would short-circuit on
	// IsApprovedFor above and the Fides half would never be attempted — the
	// Bundle would read as approved here while the change gate went on
	// reporting a missing sign-off, with nothing to retry. This way a failed
	// approval is simply not recorded, and running the same command again
	// completes it: the Fides write is an upsert keyed on the trail and the
	// approver, so repeating it is a no-op.
	if err := o.recordApprovalInFides(ctx, g, b, actor); err != nil {
		return err
	}

	approval := v1alpha1.BundleApproval{Gate: gateName, Actor: actor, At: o.now()}

	// Retried rather than patched: this appends to a list, and a merge patch
	// would replace the whole list with the one we computed — silently dropping
	// an approval recorded for another Gate in between.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := o.Bundle(ctx, namespace, bundleName)
		if err != nil {
			return err
		}
		if fresh.IsApprovedFor(gateName) {
			return nil
		}
		fresh.Status.ApprovedFor = append(fresh.Status.ApprovedFor, approval)
		return o.Client.Status().Update(ctx, fresh)
	}); err != nil {
		return fmt.Errorf("recording approval: %w", err)
	}
	return nil
}

// recordApprovalInFides tells the compliance system who signed off, so its
// segregation-of-duties evaluation has an approver identity to compare against
// the committer and the deployer.
//
// A Gate that names no Fides environment is not using any of this, and this is
// a no-op. Everything else is a hard failure rather than a warning: an approval
// that only half-landed leaves the Bundle looking approved in the cluster while
// the change gate still holds, and the operator has no way to tell which half
// is missing. Refusing outright means "run it again" is always the right answer.
func (o *Ops) recordApprovalInFides(
	ctx context.Context, g *v1alpha1.Gate, b *v1alpha1.Bundle, actor string,
) error {
	if g.Spec.Evidence == nil {
		return nil
	}

	c, err := passage.DialFides(ctx, o.Client, g.Namespace, o.FidesServer, g.Spec.Evidence, o.DialFides)
	if err != nil {
		return fmt.Errorf("reaching Fides to record the approval: %w", err)
	}

	digests := passage.ImageDigests(b)
	if len(digests) == 0 {
		return &RefusedError{Action: "approve", Reason: fmt.Sprintf(
			"Gate %s checks evidence, but Bundle %s pins no image digest — there is nothing to "+
				"record an approval against", g.Name, b.Name)}
	}
	trail, err := c.TrailForArtifact(ctx, digests[0])
	if err != nil {
		return fmt.Errorf("looking up the artifact's trail: %w", err)
	}
	if trail == "" {
		return &RefusedError{Action: "approve", Reason: fmt.Sprintf(
			"Fides has no record of %s, so an approval has nothing to attach to — the build that "+
				"produced it did not report the artifact", digests[0])}
	}

	err = c.RecordApproval(ctx, trail, fides.Approval{
		By:     actor,
		Role:   fides.RoleApprover,
		Reason: fmt.Sprintf("approved for Gate %s in %s", g.Name, g.Namespace),
	})
	if err != nil {
		// Refused rather than failed: Fides took the approval, it simply will
		// not count it, and telling someone their sign-off landed when the gate
		// will keep holding is the worst of the available answers (#132).
		if fides.IsUncounted(err) {
			return &RefusedError{Action: "approve", Reason: err.Error()}
		}
		return fmt.Errorf("recording the approval in Fides: %w", err)
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

	// A merge patch rather than read-modify-write. A running Passage is being
	// updated by its controller continuously, so an Update loses the race often
	// enough to matter — the first real abort against a cluster failed with
	// "the object has been modified". Setting one idempotent field needs no
	// resourceVersion, so there is nothing to conflict with.
	patch := []byte(`{"spec":{"abort":true}}`)
	if err := o.Client.Patch(ctx, p, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("aborting Passage: %w", err)
	}
	return nil
}

// Poll asks a Beacon to look at its sources now, rather than at its next
// interval.
//
// **This is the whole of the webhook receiver (#102).** A Beacon already polls
// immediately when Flux's `reconcile.fluxcd.io/requestedAt` annotation changes,
// and that path is proven end to end. What a git host could not do was set a
// Kubernetes annotation — so the missing piece was an HTTP door onto the
// mechanism, not a mechanism.
//
// It needs no shared secret and no HMAC verification, which is the posture Flux
// v2.9 moved to with OIDC-secured Receivers. The API server authenticates this
// call the same way it authenticates every other one, by asking Kubernetes to
// review the bearer token; a cluster configured to trust a CI provider's OIDC
// issuer therefore accepts that provider's workload token here with nothing
// added. A secret nobody stores is a secret nobody leaks.
//
// The token returned is echoed back in `status.lastHandledReconcileAt`, so a
// caller can tell its own request apart from someone else's.
func (o *Ops) Poll(ctx context.Context, namespace, beaconName string) (string, error) {
	b, err := o.Beacon(ctx, namespace, beaconName)
	if err != nil {
		return "", err
	}
	if b.Spec.Suspend {
		// The Beacon would acknowledge the request and poll nothing, which
		// reads as success. A suspended Beacon is a deliberate state and
		// saying so beats a 200 that changed nothing.
		return "", &RefusedError{Action: "poll", Reason: fmt.Sprintf(
			"Beacon %s is suspended, so it would acknowledge this and look at nothing", beaconName)}
	}

	// Nanoseconds, because a Beacon poked twice within the same second must see
	// two distinct values or the second request looks like the first one
	// arriving again and no reconcile is triggered.
	token := strconv.FormatInt(o.now().UnixNano(), 10)

	// A merge patch on annotations, not read-modify-write: this is one
	// idempotent field on an object a controller is updating, and the abort
	// path already learnt that Update loses that race often enough to matter.
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, v1alpha1.AnnotationReconcile, token)
	if err := o.Client.Patch(ctx, b, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return "", fmt.Errorf("asking Beacon %s to poll: %w", beaconName, err)
	}
	return token, nil
}
