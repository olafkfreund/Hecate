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
// The lookup is one filtered call: /search/artifacts takes the digest and, since
// fides#442, returns trail_id alongside it.
//
// It falls back to reading /artifacts when the response has no trail_id *key*,
// which is how a Fides predating that change answers. The distinction matters
// and is why this checks for the key rather than for a null value: an artifact
// genuinely built outside a trail also reports trail_id, as null. Treating
// "absent" and "null" alike would make an old server look like an org whose
// artifacts have no trails, and TrailForArtifact would answer "" — which the
// caller reads as "CI never registered this digest" and gates on. A silently
// wrong answer, rather than a slow one.
//
// The fallback is worth keeping until Hecate states a minimum Fides version,
// because /artifacts runs a per-row SBOM query and embeds the payload: the
// response is the size of every SBOM in the organisation, hundreds of megabytes
// reachable, to learn one 36-byte trail id. Slow is survivable; wrong is not.
func (c *Client) TrailForArtifact(ctx context.Context, sha256 string) (string, error) {
	digest := strings.TrimPrefix(strings.TrimSpace(sha256), "sha256:")
	if digest == "" {
		return "", errors.New("fides: no artifact digest")
	}

	// Decoded loosely so the presence of trail_id can be told from its value.
	var found []map[string]json.RawMessage
	if err := c.get(ctx, "api/v1/search/artifacts?sha="+url.QueryEscape(digest), &found); err != nil {
		return "", err
	}
	for _, a := range found {
		raw, ok := a["trail_id"]
		if !ok {
			// An old server. Nothing here can answer the question.
			return c.trailByScanningArtifacts(ctx, digest)
		}
		// The digest is re-checked even though the server was asked to filter
		// on it. The answer decides which trail a promotion gates on, so the
		// cost of accepting a row for a different artifact is gating on
		// somebody else's SBOM and scans — worth one comparison to refuse.
		var sha string
		if err := json.Unmarshal(a["sha256"], &sha); err == nil && !strings.EqualFold(sha, digest) {
			continue
		}
		var id *string
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", fmt.Errorf("fides: decoding trail_id: %w", err)
		}
		if id != nil && *id != "" {
			return *id, nil
		}
	}
	return "", nil
}

// trailByScanningArtifacts is the pre-fides#442 path: /artifacts carries
// trail_id but takes no filter, so the whole organisation is read and matched
// here. Kept only for servers whose /search/artifacts omits trail_id.
func (c *Client) trailByScanningArtifacts(ctx context.Context, digest string) (string, error) {
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

// UncountedError is an approval Fides stored but will never count.
//
// The change gate approves on `no failing controls, no missing evidence, and at
// least one *human* approver`, and it counts an approver as human only when the
// stored `approver_kind` is `session`. A bearer token authenticates as a
// service, so Hecate's approvals only count when Fides honours the
// `on_behalf_of` delegation — which is default-deny, needs the server run with
// FIDES_DELEGATED_APPROVAL_ENABLED=true, and needs this token to hold Admin.
//
// When it is not honoured the request still returns 201. The approval is real,
// it is simply invisible to the gate, and the promotion waits for a signature
// that has already been given. Worse, a service principal's stored identity is
// the literal string "service-account" against a UNIQUE(trail_id, approved_by)
// constraint, so an approver and a deployer recorded this way collapse into one
// row and four-eyes can never be satisfied.
//
// None of that is visible from the status code, which is why this exists.
type UncountedError struct {
	// Kind is what Fides stored, e.g. "service". Anything but "session" is
	// uncounted.
	Kind string
	// By is the identity the approval was meant to be attributed to.
	By string
	// Role is the role that was recorded.
	Role string
}

func (e *UncountedError) Error() string {
	return fmt.Sprintf(
		"fides: the %s approval for %s was stored as %q rather than \"session\", so the change "+
			"gate will not count it and the change stays held. Fides honours an on_behalf_of "+
			"approval only when the server runs with FIDES_DELEGATED_APPROVAL_ENABLED=true, the "+
			"token holds the Admin role, and %s is a registered user in the organisation",
		e.Role, e.By, e.Kind, e.By)
}

// IsUncounted reports whether an approval was stored but will never be counted.
func IsUncounted(err error) bool {
	var e *UncountedError
	return errors.As(err, &e)
}

// RecordApproval records a sign-off on a trail, so Fides can evaluate
// segregation of duties over the identities involved.
//
// **What the change gate actually checks** is narrower than it looks, and
// getting this wrong is easy: the verdict is `no failing controls, no missing
// evidence, and at least one human approver`. It does not compare identities.
// Pairwise distinctness of committer, approver and deployer is evaluated
// separately and only produces an attestation of type
// `segregation-of-duties` — which changes the verdict solely when some control
// lists that type among the evidence it requires.
//
// So recording both roles matters for the controls that ask for four-eyes, and
// the *count* of human approvers is what lifts a plain hold.
//
// Returns an UncountedError when Fides stored the approval in a form the gate
// will not count. That is not a transport failure and retrying will not fix it;
// see UncountedError for what will.
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

	// Fides reports on the approval itself which kind it stored, so this needs
	// no second call to find out whether the sign-off will count.
	var out struct {
		Kind string `json:"kind"`
	}
	err := c.do(ctx, http.MethodPost, "api/v1/trails/"+url.PathEscape(trail)+"/approvals",
		map[string]any{
			"role":         a.Role,
			"on_behalf_of": a.By,
			"reason":       a.Reason,
		}, &out)
	if err != nil {
		return err
	}
	// An older Fides that does not report a kind is left alone rather than
	// guessed at: reporting every approval as uncounted would be worse than the
	// silence this replaces.
	if out.Kind != "" && out.Kind != approvalKindSession {
		return &UncountedError{Kind: out.Kind, By: a.By, Role: a.Role}
	}
	return nil
}

// approvalKindSession is the stored kind the change gate counts as human.
const approvalKindSession = "session"
