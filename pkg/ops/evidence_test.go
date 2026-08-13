package ops

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// changeGate teaches the fake Fides to answer the change-gate read, with the
// body evidance-vault's computeChangeGate actually produces.
const heldVerdict = `{"trail_id":"t","recommendation":"hold","approved":false,"risk_score":45,
"risk_level":"medium","passed":["CC8.1"],
"failed":[{"control":"CC7.2","name":"Vulnerability scanning","reasons":["failed vuln-scan"]}],
"missing_evidence":[],"waived":[],"attestations":{"total":4,"non_compliant":1},
"approvals":{"count":1,"human_approvers":1,"four_eyes":false,"approvers":["olaf@acme.example"],"deployers":[]},
"segregation_of_duties":{"committer":"olaf@acme.example","approvers":["olaf@acme.example"],
"deployers":[],"compliant":false,"violations":["committer and approver are the same person"]},
"summary":"Controls failed."}`

func TestEvidenceAnswersWhoAllowedItAndWhy(t *testing.T) {
	f := &sodFides{trail: sodTrail, changeGate: heldVerdict}
	server := f.start(t)
	o, _ := evidenceOps(t,
		testGate("staging", withEvidence(server)), testBundle("b1", 0, withDigest), sodSecret())

	ev, err := o.Evidence(context.Background(), "acme", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Trail != sodTrail {
		t.Errorf("trail = %q, want the artifact's", ev.Trail)
	}
	if ev.Verdict == nil {
		t.Fatal("no verdict — the panel would show a Bundle with no explanation of why it is held")
	}
	if ev.Verdict.RiskScore != 45 || ev.Verdict.Recommendation != "hold" {
		t.Errorf("verdict = %+v", ev.Verdict)
	}
	// The reasons are the actionable half: "CC7.2" is a code to look up.
	if got := ev.Verdict.Blockers(); len(got) != 1 || !strings.Contains(got[0], "failed vuln-scan") {
		t.Errorf("blockers = %v, want the control's own reason", got)
	}
	// "Who allowed it" is the other half of the question, and four-eyes here is
	// false because one person did both — which is the finding, not a gap.
	if ev.Verdict.SoD == nil || ev.Verdict.SoD.Compliant {
		t.Errorf("sod = %+v, want the four-eyes violation reported", ev.Verdict.SoD)
	}
	if n := ev.Verdict.Attestations.Total; n != 4 {
		t.Errorf("attestations total = %d, want 4", n)
	}
}

// Nothing to show and everything is fine are opposite answers, and a panel that
// renders empty for both says neither.
func TestEvidenceSaysWhyThereIsNone(t *testing.T) {
	t.Run("no Gate uses Fides", func(t *testing.T) {
		o, _ := evidenceOps(t, testGate("staging"), testBundle("b1", 0, withDigest))
		ev, err := o.Evidence(context.Background(), "acme", "b1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ev.Unavailable, "records evidence in Fides") {
			t.Errorf("unavailable = %q", ev.Unavailable)
		}
	})

	t.Run("the Bundle pins no digest", func(t *testing.T) {
		f := &sodFides{trail: sodTrail, changeGate: heldVerdict}
		o, _ := evidenceOps(t,
			testGate("staging", withEvidence(f.start(t))), testBundle("b1", 0), sodSecret())
		ev, err := o.Evidence(context.Background(), "acme", "b1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ev.Unavailable, "no image digest") {
			t.Errorf("unavailable = %q", ev.Unavailable)
		}
	})

	t.Run("Fides has never seen the artifact", func(t *testing.T) {
		f := &sodFides{changeGate: heldVerdict} // no trail
		o, _ := evidenceOps(t,
			testGate("staging", withEvidence(f.start(t))), testBundle("b1", 0, withDigest), sodSecret())
		ev, err := o.Evidence(context.Background(), "acme", "b1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ev.Unavailable, "did not report the artifact") {
			t.Errorf("unavailable = %q", ev.Unavailable)
		}
		if ev.Verdict != nil {
			t.Error("a verdict was reported for an artifact Fides has never seen")
		}
	})
}

// A Fides that is configured and then does not answer is worth retrying, so it
// must not be reported as "no evidence" — that reads as a clean bill of health.
func TestEvidenceFailsLoudlyWhenFidesIsDown(t *testing.T) {
	f := &sodFides{trail: sodTrail, changeGateStatus: http.StatusInternalServerError}
	o, _ := evidenceOps(t,
		testGate("staging", withEvidence(f.start(t))), testBundle("b1", 0, withDigest), sodSecret())

	if _, err := o.Evidence(context.Background(), "acme", "b1"); err == nil {
		t.Fatal("a Fides outage was reported as an absence of evidence")
	}
}
