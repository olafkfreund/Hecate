package ops

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

func at(offset time.Duration) metav1.Time {
	return metav1.Time{Time: base.Add(offset)}
}

// The entry an audit exists for.
//
// A trail of everything that shipped is a deployment log. What makes it an
// audit trail is that it also holds what was STOPPED — and a refused crossing
// never enters a Gate, so it appears nowhere in Gate history. Building the page
// from history alone would silently omit exactly the events an auditor came to
// see, and would look complete while doing it.
func TestAuditKeepsWhatWasRefused(t *testing.T) {
	failed := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-abc", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "production", Bundle: "app-1", Actor: "someone@example.com"},
		Status: v1alpha1.PassageStatus{
			Phase:      v1alpha1.PassageFailed,
			Message:    "a step failed",
			FinishedAt: ptr(at(time.Minute)),
			Steps: []v1alpha1.StepStatus{
				{Uses: "evidence-gate", Phase: v1alpha1.StepFailed,
					Message: "not compliant: segregation-of-duties"},
			},
		},
	}
	o, _ := newOps(t, gateNamed("production"), failed)

	entries, err := o.Audit(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}

	var refusal *AuditEntry
	for i := range entries {
		if entries[i].Kind == AuditRefused {
			refusal = &entries[i]
		}
	}
	if refusal == nil {
		t.Fatal("the refused crossing is absent — it never entered the Gate, so history cannot hold it")
	}
	// The failing step's own words, not the Passage's summary. "a step failed"
	// tells an auditor nothing; the control that refused is the answer.
	if !strings.Contains(refusal.Detail, "segregation-of-duties") {
		t.Errorf("detail = %q, want the failing step's reason", refusal.Detail)
	}
	if refusal.Actor != "someone@example.com" {
		t.Errorf("actor = %q, want who attempted it", refusal.Actor)
	}
}

// History outlives the Passage it names, so a crossing must survive its
// Passage being collected — losing the record of what shipped because a
// detail object aged out would be the worst possible retention behaviour.
func TestAuditKeepsCrossingsWhoseDetailHasAgedOut(t *testing.T) {
	gate := gateNamed("staging")
	gate.Status.History = []v1alpha1.GateOccupant{{
		Bundle: "app-1", Digest: "sha256:abc", Passage: "staging-gone",
		EnteredAt: at(0), Actor: "auto",
	}}
	o, _ := newOps(t, gate)

	entries, err := o.Audit(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != AuditCrossed {
		t.Fatalf("got %d entries %+v, want one crossing", len(entries), entries)
	}
	if entries[0].Digest != "sha256:abc" {
		t.Errorf("digest = %q — the Bundle name is a label, the digest is which bits shipped", entries[0].Digest)
	}
}

// A crossing recorded in both places is one event, not two.
func TestAuditDoesNotReportACrossingTwice(t *testing.T) {
	gate := gateNamed("staging")
	gate.Status.History = []v1alpha1.GateOccupant{{
		Bundle: "app-1", Passage: "staging-1", EnteredAt: at(0),
	}}
	p := &v1alpha1.Passage{
		ObjectMeta: metav1.ObjectMeta{Name: "staging-1", Namespace: "acme"},
		Spec:       v1alpha1.PassageSpec{Gate: "staging", Bundle: "app-1"},
		Status: v1alpha1.PassageStatus{
			Phase: v1alpha1.PassageSucceeded, FinishedAt: ptr(at(0)),
			Evidence: &v1alpha1.EvidenceRef{Trail: "t-1", Verdict: "approve"},
		},
	}
	o, _ := newOps(t, gate, p)

	entries, err := o.Audit(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 — the Passage and the history entry are the same crossing", len(entries))
	}
	// And the history entry borrowed the Passage's evidence, which is the link
	// from "it was promoted" to "here is what said it could be".
	if entries[0].Evidence == nil || entries[0].Evidence.Trail != "t-1" {
		t.Errorf("evidence = %+v, want the Passage's trail", entries[0].Evidence)
	}
}

func ptr(t metav1.Time) *metav1.Time { return &t }

func gateNamed(name string) *v1alpha1.Gate {
	return &v1alpha1.Gate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"}}
}
