//go:build fides

// Transitions, as opposed to states.
//
// fides_test.go checks every answer Fides gives against a real server, but it
// only ever reads. That confirms we parse what Fides sends; it does not confirm
// the two things the gates exist to do (#113):
//
//  1. a held change is released once someone approves it, and the waiting
//     crossing then succeeds — the whole reason a hold returns Running rather
//     than failing (D30);
//  2. an allowlist refusal is cleared by allowlisting the digest.
//
// The second is proven outright. The first is proven as far as an organisation
// with adopted controls allows: the approval registers as human sign-off, which
// is the part Hecate is responsible for, and the release is then reported as
// unproven rather than faked when the organisation's own controls are still
// holding the change on evidence a synthetic trail cannot produce. The skip
// says which. See #113.
//
// Both write, which is why they are opt-in rather than part of `make
// fides-test`. Set FIDES_SCRATCH_FLOW to the name of a flow you are willing to
// have written to:
//
//	FIDES_SERVER_URL=… FIDES_TOKEN=… FIDES_SCRATCH_FLOW=hecate-integration \
//	  go test -tags fides -count=1 -v -run Transition ./test/integration/...
//
// WHAT THIS LEAVES BEHIND, because it cannot do otherwise. Fides has no delete
// endpoint for trails, artifacts, attestations or approvals — attestations are
// hash-chained and append-only by design, and the rest simply have no route.
// The allowlist entry is the one exception and is removed in a Cleanup. So each
// run leaves one trail and one artifact in the scratch flow, permanently.
//
// A fresh trail per run is not fussiness: an approval cannot be withdrawn, so a
// trail that has already been approved starts approved, and the hold-then-
// release transition is observable exactly once per trail. Reusing one would
// prove the transition on the first run and nothing on every run after.
//
// Point this at a scratch flow, never at the flow recording real releases.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/passage"
	"github.com/olafkfreund/hecate/pkg/passage/steps"
)

// The two identities the approvals are recorded on behalf of, plus the
// committer the trail is tagged with. Three distinct people, because that is
// what segregation of duties means — and because Fides evaluates committer
// against approver against deployer pairwise.
//
// They are addresses at example.com on purpose: RFC 2606 reserves it, so
// registering them as users cannot collide with a real person's account.
const (
	committer = "committer@example.com"
	approver  = "approver@example.com"
	deployer  = "deployer@example.com"
)

func scratchFlow(t *testing.T) string {
	t.Helper()
	flow := os.Getenv("FIDES_SCRATCH_FLOW")
	if flow == "" {
		t.Skip("set FIDES_SCRATCH_FLOW to a flow you are willing to have written to — " +
			"these tests create a trail that cannot be deleted afterwards")
	}
	return flow
}

// post is a raw write, the counterpart to get. The client deliberately exposes
// no way to create a flow or a trail: Hecate gates on the trail CI already
// built, and a promotion that opens its own trail would be gating on evidence
// it wrote itself. Setting up a fixture is a thing this test needs and a
// crossing does not.
func post(ctx context.Context, t *testing.T, path string, body, out any) error {
	t.Helper()
	return send(ctx, t, http.MethodPost, path, body, out)
}

func send(ctx context.Context, t *testing.T, method, path string, body, out any) error {
	t.Helper()
	var payload *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = strings.NewReader(string(encoded))
	}

	base := strings.TrimSuffix(os.Getenv("FIDES_SERVER_URL"), "/")
	var req *http.Request
	var err error
	if payload != nil {
		req, err = http.NewRequestWithContext(ctx, method, base+"/"+path, payload)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, base+"/"+path, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("FIDES_TOKEN"))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return &fides.Error{Status: resp.StatusCode, Method: method, Path: path}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// fixture is a trail nobody has approved yet, with an artifact on it.
//
// Fresh every run — see the package comment for why reuse cannot work.
func fixture(t *testing.T, c *fides.Client) subject {
	t.Helper()
	ctx := context.Background()
	flow := scratchFlow(t)

	// The flow is reused across runs. Creating it is a POST rather than an
	// upsert, so look first — a 409 here would be a confusing way to learn the
	// flow already exists.
	var flows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := get(ctx, t, "api/v1/flows", &flows); err != nil {
		t.Fatalf("listing flows: %v", err)
	}
	var flowID string
	for _, f := range flows {
		if f.Name == flow {
			flowID = f.ID
			break
		}
	}
	if flowID == "" {
		var created struct {
			ID string `json:"id"`
		}
		body := map[string]any{"name": flow, "description": "Hecate's integration tests — scratch, not real evidence"}
		if err := post(ctx, t, "api/v1/flows", body, &created); err != nil {
			t.Fatalf("creating the scratch flow %q: %v", flow, err)
		}
		flowID = created.ID
	}

	// Unique per run, and named so that anyone finding one of these in Fides
	// later knows what wrote it and can tell it from real evidence.
	name := fmt.Sprintf("hecate-transition-%d", time.Now().UnixNano())

	var trail struct {
		ID string `json:"id"`
	}
	// The committer tag is what segregation of duties reads to identify who
	// wrote the change; without it the SoD finding has only two of its three
	// identities and reports incomplete rather than compliant.
	err := post(ctx, t, "api/v1/trails", map[string]any{
		"flow_id": flowID,
		"name":    name,
		"tags":    map[string]string{"committer": committer, "source": "hecate-integration-test"},
	}, &trail)
	if err != nil {
		t.Fatalf("creating trail %s: %v", name, err)
	}
	if trail.ID == "" {
		t.Fatalf("trail %s was created without an id", name)
	}

	// A digest no other run will produce. Fides upserts artifacts on the
	// digest, so a shared one would re-point an earlier run's artifact at this
	// run's trail — the exact detaching ReportArtifact refuses to do.
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:])

	if err := c.ReportArtifact(ctx, fides.Artifact{
		SHA256: digest,
		Trail:  trail.ID,
		Name:   "hecate-integration",
		Type:   "docker",
	}); err != nil {
		t.Fatalf("reporting the artifact: %v", err)
	}

	// Idempotent upsert on (org, name), so this is created once and found
	// thereafter rather than accumulating.
	var env struct {
		ID string `json:"id"`
	}
	if err := post(ctx, t, "api/v1/environments", map[string]any{
		"name": "hecate-integration", "type": "k8s",
		"description": "Hecate's integration tests — scratch",
	}, &env); err != nil {
		t.Fatalf("creating the scratch environment: %v", err)
	}
	if env.ID == "" {
		t.Fatal("the scratch environment was created without an id")
	}

	t.Logf("fixture: trail %s (%s) artifact %s env %s", name, trail.ID, digest[:12], env.ID)
	return subject{Digest: digest, Trail: trail.ID, Env: env.ID}
}

// TestTransitionHeldChangeIsReleased is the behaviour in #26 that nothing
// proved: a crossing held by the change gate resumes once the change is
// approved, rather than having failed and needing to be retriggered.
//
// The hold and the release are checked by running the same step twice against
// the same trail, which is what a Passage does when it requeues.
func TestTransitionHeldChangeIsReleased(t *testing.T) {
	c := client(t)
	s := fixture(t, c)
	ctx := context.Background()
	step, sc := gateStep(t, s)

	// A trail nobody has signed off on. Fides holds it, which is the control
	// working rather than a fault.
	before, err := c.ChangeGate(ctx, s.Trail)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Held() {
		t.Fatalf("a brand-new trail was approved before anyone signed off: %+v", before)
	}
	if before.Approvals.HumanApprovers != 0 {
		t.Fatalf("a brand-new trail already has %d human approver(s)",
			before.Approvals.HumanApprovers)
	}

	res, err := step.Run(ctx, withGates(t, sc, steps.GateChange))
	if err != nil {
		t.Fatalf("a held change must not fail the crossing: %v", err)
	}
	if res.Phase != v1alpha1.StepRunning {
		t.Fatalf("phase = %s, want Running — a hold waits, it does not fail", res.Phase)
	}
	t.Logf("held, as it should be: %s", res.Message)

	// Fides refuses an on_behalf_of for an address it does not know, so the
	// identities have to exist before they can approve anything.
	//
	// Best-effort: after the first run they already exist, and how Fides
	// answers a re-registration is not what this test is about. The assertion
	// is the approval below — if registration silently did nothing that
	// mattered, RecordApproval fails and says so.
	for _, who := range []string{approver, deployer} {
		body := map[string]any{"name": who, "email": who, "role": "Writer"}
		if err := post(ctx, t, "api/v1/tenant/users", body, nil); err != nil {
			t.Logf("registering %s: %v (already registered, or this token is not Admin)", who, err)
		}
	}

	// The approval. Both roles, because Fides treats a missing role as
	// non-compliant rather than absent.
	for _, a := range []fides.Approval{
		{By: approver, Role: fides.RoleApprover, Reason: "hecate integration test"},
		{By: deployer, Role: fides.RoleDeployer, Reason: "hecate integration test"},
	} {
		if err := c.RecordApproval(ctx, s.Trail, a); err != nil {
			// Delegation is default-deny on the server and Admin-only. Without
			// it a service token's approval is recorded as kind "service",
			// which never counts as a human approver — so the hold could never
			// lift and the rest of this test would be measuring nothing.
			t.Fatalf("recording the %s approval for %s: %v\n"+
				"If this is a 400, the identity is probably not a registered user in the "+
				"organisation; if the hold below never lifts, check that the server runs "+
				"with FIDES_DELEGATED_APPROVAL_ENABLED=true and this token is Admin.",
				a.Role, a.By, err)
		}
	}

	after, err := c.ChangeGate(ctx, s.Trail)
	if err != nil {
		t.Fatal(err)
	}

	// The half that is always provable: the approvals Hecate recorded count as
	// human sign-off.
	//
	// This is the part #132 doubted. A service token's approval is stored with
	// approver_kind "service", which Fides never counts, and its approved_by is
	// the literal string "service-account" against UNIQUE(trail_id,
	// approved_by) — so on a server without on_behalf_of delegation both roles
	// collapse into one uncounted row. Seeing two counted approvers here is
	// what says delegation is on and working.
	if after.Approvals.HumanApprovers < 2 {
		t.Fatalf("human approvers = %d after recording an approver and a deployer, want 2.\n"+
			"count=%d approvers=%v deployers=%v\n"+
			"Zero means the approvals were stored as kind \"service\": Fides counts only "+
			"session-kind approvals, so on_behalf_of delegation was not honoured — it needs "+
			"FIDES_DELEGATED_APPROVAL_ENABLED=true on the server and an Admin token. #132.",
			after.Approvals.HumanApprovers, after.Approvals.Count,
			after.Approvals.Approvers, after.Approvals.Deployers)
	}
	t.Logf("approvals registered as human sign-off: count=%d human=%d approvers=%v deployers=%v",
		after.Approvals.Count, after.Approvals.HumanApprovers,
		after.Approvals.Approvers, after.Approvals.Deployers)

	// The other half needs a trail whose *only* blocker was the signature.
	//
	// Fides approves on `len(failed) == 0 && len(missing) == 0 &&
	// humanApprovers >= 1`, so on an organisation with real controls adopted, a
	// trail this test invented is held by its missing SBOM, scans and
	// provenance no matter who signs it. That is the compliance system working
	// as designed, not a defect, and the three ways to get past it — synthesise
	// evidence that satisfies each control's JQ rules, waive the controls, or
	// rewrite their rules — are all either guesswork or a change to the
	// organisation's real posture. Waivers are org-wide, so waiving here would
	// weaken the controls over Hecate's own release trails.
	//
	// So this reports rather than pretends. See #113.
	if len(after.MissingEvidence) > 0 || len(after.Failed) > 0 {
		t.Skipf("the approval registered, but this organisation's controls still hold the "+
			"change on evidence a synthetic trail cannot produce, so the release half of the "+
			"transition is unproven here. Run against an organisation with no controls "+
			"adopted, or against a trail a real build populated.\nstill outstanding: %v",
			after.Blockers())
	}

	if after.Held() {
		hint := "Evidence went missing between the check above and here, which should not happen."
		if after.Approvals.HumanApprovers == 0 {
			hint = "human_approvers is 0, so the approvals were recorded as kind \"service\". " +
				"Fides counts only session-kind approvals, so on_behalf_of delegation was not " +
				"honoured — it needs FIDES_DELEGATED_APPROVAL_ENABLED=true on the server and " +
				"an Admin token. #132."
		}
		t.Fatalf("the change is still held after both approvals were recorded: %s\n"+
			"blockers: %v\napprovals: count=%d human=%d\n%s",
			after.Summary, after.Blockers(),
			after.Approvals.Count, after.Approvals.HumanApprovers, hint)
	}

	// And the crossing that was waiting now goes through, which is the part
	// that actually matters to whoever is watching a stuck promotion.
	res, err = step.Run(ctx, withGates(t, sc, steps.GateChange))
	if err != nil {
		t.Fatalf("the approved change failed the crossing: %v", err)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("phase = %s, want Succeeded once approved (message: %s)", res.Phase, res.Message)
	}
	t.Logf("released: risk=%d four_eyes=%v approvers=%v",
		after.RiskScore, after.Approvals.FourEyes, after.Approvals.Approvers)
}

// TestTransitionAllowlistRefusalIsCleared is the other half: a digest nobody
// has approved for the environment is refused terminally, and allowlisting it
// clears the refusal.
//
// The refusal tells an operator to run `fides allowlist add`. This checks that
// doing what the message says actually works — a remedy nobody has run is a
// remedy nobody knows is wrong.
func TestTransitionAllowlistRefusalIsCleared(t *testing.T) {
	c := client(t)
	s := fixture(t, c)
	ctx := context.Background()
	step, sc := gateStep(t, s)

	approved, err := c.Allowlisted(ctx, s.Env, s.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		t.Fatalf("a digest never seen before is already allowlisted — %s", s.Digest)
	}

	_, err = step.Run(ctx, withGates(t, sc, steps.GateAllowlist))
	if !passage.IsTerminal(err) {
		t.Fatalf("err = %v, want a terminal refusal", err)
	}
	if passage.ReasonOf(err) != steps.ReasonNotAllowlisted {
		t.Fatalf("reason = %s, want %s", passage.ReasonOf(err), steps.ReasonNotAllowlisted)
	}
	t.Logf("refused, as it should be: %v", err)

	// The allowlist entry is the only thing here Fides will let us take back,
	// so it is the only thing this suite cleans up.
	path := "api/v1/environments/" + s.Env + "/allowlist"
	if err := post(ctx, t, path, map[string]any{
		"artifact_sha256": s.Digest,
		"reason":          "hecate integration test",
	}, nil); err != nil {
		t.Fatalf("allowlisting %s: %v", s.Digest[:12], err)
	}
	t.Cleanup(func() {
		if err := send(context.Background(), t, http.MethodDelete, path+"/"+s.Digest, nil, nil); err != nil {
			t.Logf("could not remove the allowlist entry for %s: %v", s.Digest[:12], err)
		}
	})

	if approved, err = c.Allowlisted(ctx, s.Env, s.Digest); err != nil || !approved {
		t.Fatalf("the digest is still not allowlisted after being added (approved=%v, %v)", approved, err)
	}

	// The same crossing, now permitted.
	res, err := step.Run(ctx, withGates(t, sc, steps.GateAllowlist))
	if err != nil {
		t.Fatalf("the allowlisted digest was still refused: %v", err)
	}
	if res.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("phase = %s, want Succeeded once allowlisted (message: %s)", res.Phase, res.Message)
	}
	t.Logf("cleared: %s", res.Message)
}
