package ops

import (
	"context"
	"fmt"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// Evidence is everything Fides holds about one Bundle's artifact, assembled to
// answer a single question: why was this allowed into production, and who
// allowed it?
//
// Answering it today means opening Fides, finding the trail, and reading four
// pages. This is that answer in one object, so a CLI can print it and the UI
// can show it without the reader leaving the page.
type Evidence struct {
	Bundle    string `json:"bundle"`
	Namespace string `json:"namespace"`
	// Digest is the artifact all of this is about. Named explicitly because the
	// evidence belongs to the image, not to the Bundle that happens to pin it.
	Digest string `json:"digest,omitempty"`
	// Trail is the Fides trail this evidence lives on. Reported as the bare
	// identifier rather than a link: the portal is a single-page export with
	// no per-trail route to deep-link to, and a link that 404s is worse than
	// an id an auditor can paste.
	Trail string `json:"trail,omitempty"`
	// Gate is which Gate's Fides configuration was used to look this up, so a
	// reader can tell which environment's view they are seeing.
	Gate string `json:"gate,omitempty"`
	// Verdict is the change gate's answer, with its controls, attestation
	// counts, approvals and segregation-of-duties finding.
	Verdict *fides.ChangeVerdict `json:"verdict,omitempty"`
	// ApprovedIn lists the approvals Hecate itself recorded, which is not the
	// same list as Fides' — a Gate that does not use Fides still has approvers,
	// and they belong in the answer to "who allowed it".
	ApprovedIn []v1alpha1.BundleApproval `json:"approvedIn,omitempty"`
	// Unavailable says why there is nothing to show, when there is nothing.
	// A panel that renders empty is indistinguishable from a clean bill of
	// health, and those are opposite answers.
	Unavailable string `json:"unavailable,omitempty"`
}

// Evidence assembles the compliance record for a Bundle.
//
// **It never fails for want of evidence.** No Gate configured for Fides, no
// digest pinned, no trail on the artifact — each is a fact about this
// deployment rather than an error, and each is reported in `unavailable` so the
// caller can say which. Only a Fides that is configured and then does not
// answer is an error, because that one is worth retrying.
func (o *Ops) Evidence(ctx context.Context, namespace, bundleName string) (*Evidence, error) {
	b, err := o.Bundle(ctx, namespace, bundleName)
	if err != nil {
		return nil, err
	}

	ev := &Evidence{
		Bundle:     bundleName,
		Namespace:  namespace,
		ApprovedIn: b.Status.ApprovedFor,
	}

	g, err := o.evidenceGate(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if g == nil {
		ev.Unavailable = "no Gate in this namespace records evidence in Fides"
		return ev, nil
	}
	ev.Gate = g.Name

	digests := passage.ImageDigests(b)
	if len(digests) == 0 {
		ev.Unavailable = fmt.Sprintf(
			"Bundle %s pins no image digest, so there is no artifact to hold evidence about", bundleName)
		return ev, nil
	}
	ev.Digest = digests[0]

	c, err := passage.DialFides(ctx, o.Client, namespace, o.FidesServer, g.Spec.Evidence, o.DialFides)
	if err != nil {
		return nil, fmt.Errorf("reaching Fides: %w", err)
	}
	trail, err := c.TrailForArtifact(ctx, ev.Digest)
	if err != nil {
		return nil, fmt.Errorf("looking up the artifact's trail: %w", err)
	}
	if trail == "" {
		ev.Unavailable = fmt.Sprintf(
			"Fides has no trail for %s — the build that produced it did not report the artifact",
			ev.Digest)
		return ev, nil
	}
	ev.Trail = trail

	verdict, err := c.ChangeGate(ctx, trail)
	if err != nil {
		return nil, fmt.Errorf("reading the change-gate verdict: %w", err)
	}
	ev.Verdict = verdict
	return ev, nil
}

// evidenceGate picks the Gate whose Fides configuration to use.
//
// The trail belongs to the artifact, not to a Gate, so any Gate configured for
// Fides in this namespace reaches the same record — which is why the first one
// will do rather than requiring the caller to name one. The Gate is reported
// back so a reader can see whose credentials answered.
func (o *Ops) evidenceGate(ctx context.Context, namespace string) (*v1alpha1.Gate, error) {
	gates, err := o.Gates(ctx, namespace)
	if err != nil {
		return nil, err
	}
	for i := range gates {
		if gates[i].Spec.Evidence != nil {
			return &gates[i], nil
		}
	}
	return nil, nil
}
