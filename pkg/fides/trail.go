package fides

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TrailForArtifact finds the trail an artifact digest was built on.
//
// The trail that matters already exists: CI opened it when it built the image
// and recorded the SBOM and scan attestations there. Those attestations are
// exactly what an environment policy and the change gate judge, so a crossing
// has to gate on *that* trail. A trail Hecate opened fresh would carry none of
// them, and both checks would refuse every promotion — a gate that always says
// no is a gate somebody switches off.
//
// Returns "" when Fides has never seen the digest, which is not an error: it
// means CI did not register the artifact, and the caller decides whether that
// is disqualifying.
//
// ponytail: Fides has no by-digest lookup that also returns the trail — its
// /search/artifacts filters by sha but omits trail_id, and /artifacts returns
// trail_id but takes no filter — so this reads the org's artifacts and matches
// here. One call per crossing, and it grows with the artifact count. The
// upgrade path is a sha filter on /artifacts upstream; tracked in #111.
func (c *Client) TrailForArtifact(ctx context.Context, sha256 string) (string, error) {
	digest := strings.TrimPrefix(strings.TrimSpace(sha256), "sha256:")
	if digest == "" {
		return "", errors.New("fides: no artifact digest")
	}

	var artifacts []struct {
		SHA256  string  `json:"sha256"`
		TrailID *string `json:"trail_id"`
	}
	if err := c.get(ctx, "api/v1/artifacts", &artifacts); err != nil {
		return "", err
	}
	for _, a := range artifacts {
		if strings.EqualFold(a.SHA256, digest) && a.TrailID != nil {
			return *a.TrailID, nil
		}
	}
	return "", nil
}

// Attestation is one piece of evidence on a trail.
type Attestation struct {
	// Name is what this evidence is, e.g. "promotion".
	Name string
	// Type is the attestation type an environment policy asks for by name, so
	// `policy check` can require it. Getting this wrong means a policy that
	// requires "deployment" never sees one.
	Type string
	// ArtifactSHA256 ties the evidence to what was deployed.
	ArtifactSHA256 string
	// Payload is the evidence body, stored and hashed into the chain.
	Payload any
	// SignedBy is who or what produced it.
	SignedBy string
}

// Attest records evidence on a trail.
func (c *Client) Attest(ctx context.Context, trail string, a Attestation) error {
	switch {
	case trail == "":
		return errors.New("fides: no trail to attest to")
	case a.Name == "" || a.Type == "":
		return errors.New("fides: an attestation needs a name and a type")
	}

	// The payload is a JSON string rather than an object: Fides stores it as
	// text and hashes the canonical form into the tamper-evidence chain.
	encoded, err := json.Marshal(a.Payload)
	if err != nil {
		return fmt.Errorf("fides: encoding the attestation payload: %w", err)
	}

	signedBy := a.SignedBy
	if signedBy == "" {
		signedBy = "hecate"
	}
	return c.do(ctx, http.MethodPost, "api/v1/attestations", map[string]any{
		"trail_id":        trail,
		"artifact_sha256": strings.TrimPrefix(a.ArtifactSHA256, "sha256:"),
		"name":            a.Name,
		"type_name":       a.Type,
		"payload":         string(encoded),
		"signed_by":       signedBy,
	}, nil)
}

// Compliance is the answer to `assert`: does this artifact satisfy a policy?
type Compliance struct {
	Compliant  bool     `json:"compliant"`
	Violations []string `json:"violations,omitempty"`
}

// Assert checks an artifact digest against a named policy. The policy may be
// empty, which asks Fides for every policy that applies.
func (c *Client) Assert(ctx context.Context, sha256, policy string) (*Compliance, error) {
	if sha256 == "" {
		return nil, errors.New("fides: no artifact digest to assert on")
	}
	q := url.Values{}
	q.Set("sha256", strings.TrimPrefix(sha256, "sha256:"))
	q.Set("policy", policy)

	var out Compliance
	if err := c.get(ctx, "api/v1/compliance?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChangeVerdict is the evidence-backed change-approval decision for a trail.
type ChangeVerdict struct {
	// Recommendation is "approve" or "hold".
	Recommendation string `json:"recommendation"`
	// Approved is Fides' own boolean, true only when every control is satisfied
	// and a human has signed off.
	Approved bool `json:"approved"`
	// RiskScore is 0-100, higher being worse.
	RiskScore int `json:"risk_score"`
	// RiskLevel is "low", "medium" or "high".
	RiskLevel string `json:"risk_level"`
	// Failed and MissingEvidence name the controls that stopped it, so a held
	// crossing can say what would unblock it.
	Failed          []string `json:"failed,omitempty"`
	MissingEvidence []string `json:"missing_evidence,omitempty"`
	// Summary is Fides' own sentence about the verdict.
	Summary string `json:"summary,omitempty"`
}

// Held reports whether the verdict withholds approval.
func (v ChangeVerdict) Held() bool { return v.Recommendation != "approve" }

// Blockers lists what is standing in the way, for a message a human can act on.
func (v ChangeVerdict) Blockers() []string {
	var out []string
	for _, f := range v.Failed {
		out = append(out, "failing control "+f)
	}
	for _, m := range v.MissingEvidence {
		out = append(out, "missing evidence "+m)
	}
	if len(out) == 0 && v.Held() {
		// Every control satisfied and still held: Fides is waiting on a human,
		// which is segregation of duties working rather than anything broken.
		out = append(out, "awaiting a human approval")
	}
	return out
}

// ChangeGate reads the change-approval verdict for a trail.
func (c *Client) ChangeGate(ctx context.Context, trail string) (*ChangeVerdict, error) {
	if trail == "" {
		return nil, errors.New("fides: no trail to gate on")
	}
	var out ChangeVerdict
	if err := c.get(ctx, "api/v1/trails/"+url.PathEscape(trail)+"/change-gate", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Artifact is a thing Fides can hold evidence about.
type Artifact struct {
	// SHA256 identifies it. The `sha256:` prefix is stripped before sending —
	// Fides keys artifacts on lowercase hex.
	SHA256 string
	// Trail links the artifact to the evidence recorded against it.
	Trail string
	// Name is human-readable, e.g. the image repository.
	Name string
	// Type is what kind of thing it is, e.g. container-image.
	Type string
	// Tags are arbitrary labels.
	Tags map[string]string
}

// ReportArtifact records an artifact against a trail.
//
// **It refuses to report without a trail, and that is a safety rule rather than
// validation.** Fides upserts on the digest with
// `ON CONFLICT (sha256) DO UPDATE SET trail_id = EXCLUDED.trail_id`, so
// reporting a digest with an empty trail would overwrite the link CI made when
// it attached the SBOM and the scans — silently detaching exactly the evidence
// a change gate exists to read. An artifact Hecate cannot link is one it should
// leave alone.
func (c *Client) ReportArtifact(ctx context.Context, a Artifact) error {
	digest := strings.TrimPrefix(strings.TrimSpace(a.SHA256), "sha256:")
	if digest == "" {
		return fmt.Errorf("reporting an artifact: no digest")
	}
	if strings.TrimSpace(a.Trail) == "" {
		return fmt.Errorf(
			"reporting artifact %s: no trail — Fides would overwrite the existing "+
				"link with an empty one, detaching the evidence CI recorded", digest)
	}

	body := map[string]any{
		"sha256":   digest,
		"trail_id": a.Trail,
		"name":     a.Name,
		"type":     a.Type,
	}
	if len(a.Tags) > 0 {
		body["tags"] = a.Tags
	}
	return c.do(ctx, http.MethodPost, "/api/v1/artifacts", body, nil)
}
