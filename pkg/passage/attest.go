package passage

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
)

// DialFides resolves the Fides server a Gate names and connects to it.
//
// Shared with the evidence-gate step, which resolves exactly the same three
// things — server, credentials Secret, token — and had the only copy until the
// controller needed to attest. Two copies of "where is Fides" is how a Gate
// ends up checked against one server and recorded on another.
//
// dial is injectable so tests can point at a fake Fides; nil means the real one.
func DialFides(
	ctx context.Context,
	c client.Client,
	namespace, defaultServer string,
	evidence *v1alpha1.EvidenceConfig,
	dial func(fides.Config) (*fides.Client, error),
) (*fides.Client, error) {
	if evidence == nil {
		return nil, fmt.Errorf("the Gate has no evidence configuration")
	}
	server := evidence.ServerURL
	if server == "" {
		server = defaultServer
	}
	if evidence.CredentialsRef == nil {
		// Named here rather than left to the client's "no API token", which
		// says nothing about where to put one.
		return nil, fmt.Errorf("the Gate has no evidence.credentialsRef — Fides rejects an " +
			"unauthenticated request, so there is no useful call to make without one")
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: evidence.CredentialsRef.Name}
	if err := c.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("reading Secret %s/%s: %w", namespace, evidence.CredentialsRef.Name, err)
	}
	token := string(secret.Data["token"])
	if token == "" {
		return nil, fmt.Errorf("the Secret %s has no token", evidence.CredentialsRef.Name)
	}

	if dial == nil {
		dial = fides.New
	}
	return dial(fides.Config{BaseURL: server, Token: token})
}

// attest records the finished crossing on the Bundle's Fides trail.
//
// **What this is for.** A Passage is the only place that knows the whole story
// — which Bundle crossed which Gate, on whose say-so, which steps ran and how
// each of them ended. Fides holds the SBOM and the scans CI attached; without
// this it never learns the artifact was actually promoted, so an auditor can
// see what was built and not what was shipped. Chained into the trail's
// tamper-evident hash chain, the crossing becomes evidence rather than a log
// line in a cluster that will be rebuilt next quarter.
//
// **Reconstructed from the persisted status**, on the same reasoning as
// recordTrace: a crossing outlives the controller process, so anything held in
// memory across it is lost by a restart. Status is what survives.
//
// **Written at most once, and never retried.** The Passage is terminal by the
// time this runs and the controller never re-runs a terminal Passage, so a
// failure here leaves a crossing with no attestation — visible as a Warning
// event and an empty `status.evidence.trail`. That is the deliberate trade: a
// retry loop against a tamper-evident chain writes duplicate entries for one
// crossing, and an auditor cannot tell a retried write from a replayed one. A
// gap that says "the evidence system was down" is honest; two chained records
// of a single promotion are not.
//
// A Gate with no `spec.evidence` is not using Fides at all, and attesting is
// skipped silently — the alternative is a warning on every crossing of every
// Gate in a cluster that has never heard of Fides.
func (r *Reconciler) attest(ctx context.Context, p *v1alpha1.Passage, bundle *v1alpha1.Bundle) {
	var gate v1alpha1.Gate
	key := client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.Gate}
	if err := r.Get(ctx, key, &gate); err != nil {
		// A deleted Gate is silence, not a warning: it is the ordinary way a
		// Passage outlives the Gate it crossed, and it is also the state of
		// every cluster that has never configured Fides. Any other read error
		// is worth saying out loud, because it may be hiding a Gate that did
		// want this crossing recorded.
		if !apierrors.IsNotFound(err) {
			r.event(p, corev1.EventTypeWarning, "AttestationSkipped",
				fmt.Sprintf("reading Gate %s to record the crossing: %s", p.Spec.Gate, err))
		}
		return
	}
	if gate.Spec.Evidence == nil {
		return
	}

	c, err := DialFides(ctx, r.Client, p.Namespace, r.FidesServer, gate.Spec.Evidence, r.DialFides)
	if err != nil {
		r.attestationFailed(p, err)
		return
	}

	digests := ImageDigests(bundle)
	trail, err := r.trail(ctx, c, p, digests)
	if err != nil {
		r.attestationFailed(p, err)
		return
	}
	if trail == "" {
		// Fides has never heard of this artifact, so there is no chain to
		// extend. Attesting to a trail we invent would be worse than not
		// attesting: it would look like evidence and be attached to nothing CI
		// produced.
		r.event(p, corev1.EventTypeWarning, "AttestationSkipped",
			fmt.Sprintf("Fides has no trail for the Bundle's artifacts, so the crossing of %s "+
				"could not be recorded — the build that produced them did not report the artifact",
				p.Spec.Gate))
		return
	}

	var artifact string
	if len(digests) > 0 {
		artifact = digests[0]
	}
	err = c.Attest(ctx, trail, fides.Attestation{
		Name:           "promotion",
		Type:           "promotion",
		ArtifactSHA256: artifact,
		SignedBy:       signer(p),
		Payload:        crossing(p, bundle, digests),
	})
	if err != nil {
		r.attestationFailed(p, err)
		return
	}

	// Recorded so `hecate verify` can find the chain this crossing is on
	// without re-deriving it from the Bundle. The evidence-gate step sets the
	// same field when it runs; this fills it in for the Passages that had no
	// such step, and leaves an existing verdict and risk score alone.
	if p.Status.Evidence == nil {
		p.Status.Evidence = &v1alpha1.EvidenceRef{}
	}
	p.Status.Evidence.Trail = trail
}

// trail is the chain this crossing belongs on.
//
// The evidence-gate step already looked it up if it ran, and that lookup is the
// one the gates were judged against — reusing it means the crossing is recorded
// on the same trail that permitted it, even in the unlikely case where the
// artifact has since been relinked.
func (r *Reconciler) trail(
	ctx context.Context, c *fides.Client, p *v1alpha1.Passage, digests []string,
) (string, error) {
	if p.Status.Evidence != nil && p.Status.Evidence.Trail != "" {
		return p.Status.Evidence.Trail, nil
	}
	if len(digests) == 0 {
		return "", nil
	}
	trail, err := c.TrailForArtifact(ctx, digests[0])
	if err != nil {
		return "", fmt.Errorf("looking up the artifact's trail: %w", err)
	}
	return trail, nil
}

// signer identifies who the promotion is attributed to.
//
// The actor when a human asked for the crossing, so segregation of duties (#28)
// has a name to check, and Hecate itself when the pipeline promoted on its own.
func signer(p *v1alpha1.Passage) string {
	if p.Spec.Actor != "" {
		return p.Spec.Actor
	}
	return "hecate"
}

// crossing is the attestation payload: everything an auditor needs to answer
// "what was promoted where, by whom, and did every check run".
//
// Plain maps rather than a struct with json tags, because this is a document
// for another system to store and display, not a type Hecate reads back. A
// struct would invite someone to unmarshal into it and start depending on the
// shape.
func crossing(p *v1alpha1.Passage, bundle *v1alpha1.Bundle, digests []string) map[string]any {
	out := map[string]any{
		"passage":   p.Name,
		"namespace": p.Namespace,
		"gate":      p.Spec.Gate,
		"bundle":    p.Spec.Bundle,
		"outcome":   string(p.Status.Phase),
		"steps":     steps(p),
	}
	if p.Spec.Actor != "" {
		out["actor"] = p.Spec.Actor
	}
	if p.Status.Message != "" {
		out["message"] = p.Status.Message
	}
	if p.Status.StartedAt != nil {
		out["startedAt"] = p.Status.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if p.Status.FinishedAt != nil {
		out["finishedAt"] = p.Status.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	// The trace this crossing belongs to, so the evidence record and the
	// telemetry are the same story told twice rather than two unrelated ones.
	if p.Status.TraceID != "" {
		out["traceID"] = p.Status.TraceID
	}
	if len(digests) > 0 {
		out["artifacts"] = digests
	}
	// The Bundle's content digest, so the record names the exact set of
	// artifacts rather than a Bundle name that could be recreated over.
	if bundle != nil && bundle.Spec.Digest != "" {
		out["bundleDigest"] = bundle.Spec.Digest
	}
	return out
}

// steps records every step, including the ones that did not run.
//
// Skipped steps are the interesting ones for an audit: "the security scan was
// skipped" is exactly the fact a record that only listed successes would hide.
func steps(p *v1alpha1.Passage) []map[string]any {
	out := make([]map[string]any, 0, len(p.Status.Steps))
	for _, st := range p.Status.Steps {
		s := map[string]any{"uses": st.Uses, "phase": string(st.Phase)}
		if st.As != "" {
			s["as"] = st.As
		}
		if st.Reason != "" {
			s["reason"] = st.Reason
		}
		if st.Message != "" {
			s["message"] = st.Message
		}
		out = append(out, s)
	}
	return out
}

func (r *Reconciler) attestationFailed(p *v1alpha1.Passage, err error) {
	r.event(p, corev1.EventTypeWarning, "AttestationFailed",
		fmt.Sprintf("the crossing of %s was not recorded in Fides: %s — the promotion happened, "+
			"the evidence did not", p.Spec.Gate, err))
}

// ImageDigests are the Bundle's pinned image digests, in spec order.
func ImageDigests(bundle *v1alpha1.Bundle) []string {
	if bundle == nil {
		return nil
	}
	var out []string
	for _, a := range bundle.Spec.Artifacts {
		if a.Image != nil && a.Image.Digest != "" {
			out = append(out, a.Image.Digest)
		}
	}
	return out
}
