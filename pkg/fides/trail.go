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
// here. One call per crossing.
//
// The ceiling is lower than "grows with the artifact count" suggests, which is
// what this comment used to say: /artifacts also runs a per-row query for each
// artifact's SBOM and embeds the payload, so the response is the size of every
// SBOM in the organisation. Hundreds of megabytes is reachable, to learn one
// 36-byte trail id.
//
// The cheap upgrade is upstream and two lines: /search/artifacts already
// filters by sha, already has LIMIT 100 and already joins trails, so adding
// a.trail_id to its SELECT is the whole change. This function then becomes one
// filtered call with no loop. Tracked in #111.
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
	// Passed names the controls that were satisfied, by key alone — Fides sends
	// strings here and objects for the two below, and that asymmetry is real.
	Passed []string `json:"passed,omitempty"`
	// Failed and MissingEvidence name the controls that stopped it, so a held
	// crossing can say what would unblock it.
	Failed          []Control `json:"failed,omitempty"`
	MissingEvidence []Control `json:"missing_evidence,omitempty"`
	// Waived are the controls a human has excused, with who excused them and
	// until when. A waiver is a governed exception rather than a pass, so it is
	// reported separately: an auditor's first question about a green gate is
	// which of it was waived.
	Waived []Control `json:"waived,omitempty"`
	// Attestations counts the evidence on the trail.
	Attestations struct {
		Total        int `json:"total"`
		NonCompliant int `json:"non_compliant"`
	} `json:"attestations"`
	// Approvals is who signed off, and whether that satisfied four-eyes.
	Approvals struct {
		Count          int      `json:"count"`
		HumanApprovers int      `json:"human_approvers"`
		FourEyes       bool     `json:"four_eyes"`
		Approvers      []string `json:"approvers,omitempty"`
		Deployers      []string `json:"deployers,omitempty"`
	} `json:"approvals"`
	// SoD is Fides' segregation-of-duties finding: committer, approver and
	// deployer must be three distinct people.
	SoD *SegregationOfDuties `json:"segregation_of_duties,omitempty"`
	// Summary is Fides' own sentence about the verdict.
	Summary string `json:"summary,omitempty"`
}

// Control is one control the change gate judged.
//
// Sent as an object rather than a bare key because the reasons are the useful
// part: "CC7.2" is a code to look up, "CC7.2 Vulnerability scanning: failed
// vuln-scan" is a thing to go and fix.
type Control struct {
	Key  string `json:"control"`
	Name string `json:"name"`
	// Reasons are Fides' own phrasings, e.g. "missing sbom".
	Reasons []string `json:"reasons,omitempty"`
	// WaivedReasons is what the waiver excused, present only on Waived.
	WaivedReasons []string `json:"waived_reasons,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	ApprovedBy    string   `json:"approved_by,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
}

// Describe names the control the way a person would read it.
func (c Control) Describe() string {
	label := c.Key
	if c.Name != "" {
		label = fmt.Sprintf("%s %s", c.Key, c.Name)
	}
	reasons := c.Reasons
	if len(reasons) == 0 {
		reasons = c.WaivedReasons
	}
	if len(reasons) == 0 {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, strings.Join(reasons, ", "))
}

// SegregationOfDuties is Fides' four-eyes finding for a trail.
type SegregationOfDuties struct {
	Committer  string   `json:"committer,omitempty"`
	Approvers  []string `json:"approvers,omitempty"`
	Deployers  []string `json:"deployers,omitempty"`
	Compliant  bool     `json:"compliant"`
	Violations []string `json:"violations,omitempty"`
}

// Held reports whether the verdict withholds approval.
func (v ChangeVerdict) Held() bool { return v.Recommendation != "approve" }

// Blockers lists what is standing in the way, for a message a human can act on.
func (v ChangeVerdict) Blockers() []string {
	var out []string
	for _, f := range v.Failed {
		out = append(out, "failing control "+f.Describe())
	}
	for _, m := range v.MissingEvidence {
		out = append(out, "missing evidence for "+m.Describe())
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

// Approval roles, as Fides names them when it evaluates segregation of duties.
const (
	// RoleApprover is a reviewer's sign-off.
	RoleApprover = "approver"
	// RoleDeployer is the identity that triggers the deployment. Distinct from
	// the approver on purpose: Fides refuses a trail where they are the same
	// person, which is the whole point of four-eyes.
	RoleDeployer = "deployer"
)

// Approval is one identity signing off on a trail in one role.
type Approval struct {
	// By is the human the approval belongs to. Required.
	//
	// Sent as `on_behalf_of`, because Hecate authenticates to Fides with a
	// service token: without it every approval Hecate records would carry the
	// service account's identity, every identity would be equal, and
	// segregation of duties would evaluate one person having done everything.
	// Fides honours the delegation only when it is configured to and the email
	// is a known user in the organisation — so this can be refused, and a
	// refusal is a real answer rather than a transport failure.
	By string
	// Role is RoleApprover or RoleDeployer.
	Role string
	// Reason is free text stored with the approval.
	Reason string
}

// RecordApproval records a sign-off on a trail, so Fides can evaluate
// segregation of duties over the identities involved.
//
// Fides derives its verdict from the trail's committer tag plus the recorded
// approvals: committer, approver and deployer must be three distinct people. It
// treats a missing role as non-compliant rather than absent, so recording only
// one of them leaves the change gate holding for a reason that reads like a
// policy failure.
func (c *Client) RecordApproval(ctx context.Context, trail string, a Approval) error {
	switch {
	case strings.TrimSpace(trail) == "":
		return errors.New("fides: no trail to record an approval on")
	case strings.TrimSpace(a.By) == "":
		// Fides would happily attribute this to the service token, and an
		// approval every promotion shares is not an approval.
		return errors.New("fides: an approval must name the human who gave it")
	case a.Role != RoleApprover && a.Role != RoleDeployer:
		return fmt.Errorf("fides: %q is not an approval role (want %s or %s)",
			a.Role, RoleApprover, RoleDeployer)
	}

	return c.do(ctx, http.MethodPost, "api/v1/trails/"+url.PathEscape(trail)+"/approvals",
		map[string]any{
			"role":         a.Role,
			"on_behalf_of": a.By,
			"reason":       a.Reason,
		}, nil)
}
