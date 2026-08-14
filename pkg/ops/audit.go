package ops

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// AuditKind is what happened.
type AuditKind string

const (
	// AuditCrossed is a Bundle that reached a Gate.
	AuditCrossed AuditKind = "crossed"
	// AuditRefused is a crossing that was attempted and did not complete.
	//
	// The most valuable entry on the page. A trail of everything that shipped
	// is a deployment log; what makes it an audit trail is that it also holds
	// what was stopped, by what, and on whose evidence.
	AuditRefused AuditKind = "refused"
	// AuditRunning is a crossing in progress.
	AuditRunning AuditKind = "running"
	// AuditApproved is a human approving a Bundle for a Gate. Separate from a
	// crossing because approving and crossing are separate acts by separate
	// people — an approval a promoter can grant themselves is not an approval.
	AuditApproved AuditKind = "approved"
)

// AuditEntry is one thing that happened, in terms an auditor asks about.
type AuditEntry struct {
	At     metav1.Time `json:"at"`
	Kind   AuditKind   `json:"kind"`
	Gate   string      `json:"gate"`
	Bundle string      `json:"bundle,omitempty"`
	// Digest is what actually shipped. The Bundle name is a label; this is the
	// content address, and it is the only field that answers "which bits".
	Digest string `json:"digest,omitempty"`
	// Actor is who caused it. Empty means nobody did — an automatic Gate acting
	// on its own, which is a meaningful answer rather than a missing one.
	Actor   string `json:"actor,omitempty"`
	Passage string `json:"passage,omitempty"`
	// Detail is why, for a refusal, in the words of whatever refused.
	Detail string `json:"detail,omitempty"`
	// Verified records whether a crossing's verification confirmed it worked,
	// which is a different question from whether it completed.
	Verified *bool `json:"verified,omitempty"`
	// Evidence links the entry to the compliance record: the Fides trail, the
	// verdict, and any blockers. This is what turns "it was promoted" into
	// "it was promoted, and here is what said it could be".
	Evidence *v1alpha1.EvidenceRef `json:"evidence,omitempty"`
}

// Audit reconstructs what has happened in a namespace, newest first.
//
// Built from two sources that outlive each other differently, which is the
// reason it is not simply a list of Passages.
//
// A Passage is the detailed record — actor, per-step outcome, the evidence
// verdict, why it stopped — and it is subject to retention: a Gate keeps a
// bounded number, so the detail ages out. `Gate.status.history` is the durable
// one, capped but long-lived, and it survives the Passage it names.
//
// So history is the spine and Passages enrich it, and a Passage with no history
// entry still appears — that is exactly the refused crossing, which never
// entered the Gate and therefore was never recorded there.
func (o *Ops) Audit(ctx context.Context, namespace string) ([]AuditEntry, error) {
	gates, err := o.Gates(ctx, namespace)
	if err != nil {
		return nil, err
	}
	passages, err := o.Passages(ctx, namespace, "", "")
	if err != nil {
		return nil, err
	}
	bundles, err := o.Bundles(ctx, namespace)
	if err != nil {
		return nil, err
	}

	// Passages indexed by name so a history entry can borrow their detail.
	byName := make(map[string]*v1alpha1.Passage, len(passages))
	for i := range passages {
		byName[passages[i].Name] = &passages[i]
	}

	entries := []AuditEntry{}
	recorded := map[string]struct{}{}

	for i := range gates {
		g := &gates[i]
		for _, occ := range g.Status.History {
			e := AuditEntry{
				At: occ.EnteredAt, Kind: AuditCrossed, Gate: g.Name,
				Bundle: occ.Bundle, Digest: occ.Digest, Actor: occ.Actor,
				Passage: occ.Passage, Verified: occ.Verified,
			}
			// The Passage carries the evidence reference; history does not, so
			// a crossing whose Passage has aged out keeps the fact and loses
			// the link. Saying less is better than implying the link was never
			// there.
			if p := byName[occ.Passage]; p != nil {
				e.Evidence = p.Status.Evidence
			}
			if occ.Passage != "" {
				recorded[occ.Passage] = struct{}{}
			}
			entries = append(entries, e)
		}
	}

	for i := range passages {
		p := &passages[i]
		if _, done := recorded[p.Name]; done {
			// Already present from history, with the outcome the Gate recorded.
			continue
		}
		e := AuditEntry{
			Gate: p.Spec.Gate, Bundle: p.Spec.Bundle, Actor: p.Spec.Actor,
			Passage: p.Name, Detail: p.Status.Message, Evidence: p.Status.Evidence,
		}
		switch p.Status.Phase {
		case v1alpha1.PassageSucceeded:
			// Succeeded but absent from history: the Gate has not written it
			// yet, or its history has rolled past it.
			e.Kind = AuditCrossed
		case v1alpha1.PassageFailed, v1alpha1.PassageAborted:
			e.Kind = AuditRefused
			if d := firstFailure(p); d != "" {
				// The step that stopped it says more than the Passage summary:
				// "evidence-gate: not compliant: segregation-of-duties" rather
				// than "a step failed".
				e.Detail = d
			}
		default:
			e.Kind = AuditRunning
		}
		e.At = timeOf(p)
		entries = append(entries, e)
	}

	for i := range bundles {
		b := &bundles[i]
		for _, a := range b.Status.ApprovedFor {
			entries = append(entries, AuditEntry{
				At: a.At, Kind: AuditApproved, Gate: a.Gate,
				Bundle: b.Name, Digest: b.Spec.Digest, Actor: a.Actor,
			})
		}
	}

	// Newest first: an audit is nearly always a question about what happened
	// recently, and the answer should not need scrolling.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[j].At.Before(&entries[i].At)
	})
	return entries, nil
}

// firstFailure is the step that stopped a Passage.
func firstFailure(p *v1alpha1.Passage) string {
	for _, s := range p.Status.Steps {
		if s.Phase == v1alpha1.StepFailed && s.Message != "" {
			return s.Message
		}
	}
	return ""
}

// timeOf is when a Passage happened, preferring the end.
//
// A finished crossing is dated by when it finished, because that is when its
// effect landed. One still running is dated by its start, so it sorts among the
// recent rather than at the bottom with a zero time.
func timeOf(p *v1alpha1.Passage) metav1.Time {
	switch {
	case p.Status.FinishedAt != nil:
		return *p.Status.FinishedAt
	case p.Status.StartedAt != nil:
		return *p.Status.StartedAt
	default:
		return p.CreationTimestamp
	}
}
