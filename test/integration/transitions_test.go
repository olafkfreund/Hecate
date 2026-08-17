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

	// A trail nobody has signed off on. Fides holds it because a human
	// approval is missing, which is the control working rather than a fault.
	before, err := c.ChangeGate(ctx, s.Trail)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Held() {
		t.Fatalf("a brand-new trail was approved before anyone signed off: %+v", before)
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

	// The transition itself.
	after, err := c.ChangeGate(ctx, s.Trail)
	if err != nil {
		t.Fatal(err)
	}
	if after.Held() {
		t.Fatalf("the change is still held after both approvals were recorded: %s\n"+
			"blockers: %v\napprovals: count=%d human=%d\n"+
			"A human_approvers of 0 here means the approvals landed as kind \"service\": "+
			"Fides only counts session-kind approvals, so on_behalf_of delegation was not "+
			"honoured (needs FIDES_DELEGATED_APPROVAL_ENABLED=true and an Admin token). #132.",
			after.Summary, after.Blockers(), after.Approvals.Count, after.Approvals.HumanApprovers)
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
