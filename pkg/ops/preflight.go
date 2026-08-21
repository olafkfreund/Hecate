package ops

import (
	"context"
	"sort"

	"github.com/olafkfreund/hecate/pkg/fides"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// Preflight is what the evidence gate would say about a Bundle, asked before
// anyone presses Cross.
//
// The evidence gate already answers this — as a step, inside a Passage, after
// the crossing has started. So the way to find out a Bundle is missing an
// attestation is to try to promote it and read the failure:
//
//	podtato-head-fa9 → production   Failed
//	evidence-gate: ... is not compliant: Failing control: segregation-of-duties
//
// That leaves a failed Passage in the record for something nobody could have
// known, and the record is the product. Asking first costs one call to Fides
// and changes the failure into a reason the button is not worth pressing.
type Preflight struct {
	Bundle string `json:"bundle"`
	// Compliant is whether the evidence gate would let it through.
	Compliant bool `json:"compliant"`
	// Missing is the attestation types the policies wanted and the trail does
	// not have, deduplicated across policies — one missing type failing four
	// policies is one thing to fix, not four.
	Missing []string `json:"missing,omitempty"`
	// Policies names the policies that are not satisfied. Carried alongside
	// Missing rather than instead of it, because they answer different
	// questions: which rule stopped this, and what has to exist for it not to.
	Policies []string `json:"policies,omitempty"`
	// Unknown says why there is no answer, when there is none. A Bundle whose
	// evidence could not be checked is not a Bundle that passed, and rendering
	// the two the same way is how a page starts lying.
	Unknown string `json:"unknown,omitempty"`
}

// Preflight asks the evidence gate about every Bundle eligible for a Gate.
//
// Its own call rather than part of Explain, and that is a cost decision: Explain
// is loaded on every page view and again on every live update, while this is one
// Fides round-trip per eligible Bundle. Folding it in would make the Gate page's
// refresh rate the rate at which Hecate polls Fides.
//
// A Gate with no evidence configuration answers with an empty list rather than
// an error: most Gates do not gate on evidence, and a screen that reported that
// as a failure would be wrong about almost every Gate.
func (o *Ops) Preflight(ctx context.Context, namespace, gateName string) ([]Preflight, error) {
	g, err := o.Gate(ctx, namespace, gateName)
	if err != nil {
		return nil, err
	}
	out := []Preflight{}
	if g.Spec.Evidence == nil || g.Spec.Evidence.FidesEnvironment == "" {
		return out, nil
	}
	eligible := g.Status.Eligible
	if len(eligible) == 0 {
		return out, nil
	}

	c, err := passage.DialFides(ctx, o.Client, namespace, o.FidesServer, g.Spec.Evidence, o.DialFides)
	if err != nil {
		// One answer for the whole list: the Gate's evidence server is
		// unreachable, which is true of every Bundle and is not a fact about
		// any of them.
		return unknownForAll(eligible, "Fides is not reachable: "+err.Error()), nil
	}

	for _, name := range eligible {
		out = append(out, o.preflightOne(ctx, c, namespace, name, g.Spec.Evidence.FidesEnvironment))
	}
	return out, nil
}

func (o *Ops) preflightOne(
	ctx context.Context, c *fides.Client, namespace, bundleName, environment string,
) Preflight {
	p := Preflight{Bundle: bundleName}

	b, err := o.Bundle(ctx, namespace, bundleName)
	if err != nil {
		p.Unknown = "this Bundle could not be read"
		return p
	}
	digests := passage.ImageDigests(b)
	if len(digests) == 0 {
		p.Unknown = "pins no image digest, so there is no artifact to hold evidence about"
		return p
	}
	trail, err := c.TrailForArtifact(ctx, digests[0])
	if err != nil {
		p.Unknown = "looking up the artifact's trail: " + err.Error()
		return p
	}
	if trail == "" {
		// Not a refusal. Nothing has attested this artifact yet, which is what
		// a Bundle looks like before its pipeline has finished reporting.
		p.Unknown = "no Fides trail for this artifact yet"
		return p
	}

	verdict, err := c.PolicyCheck(ctx, environment, trail)
	if err != nil {
		p.Unknown = "asking Fides: " + err.Error()
		return p
	}

	p.Compliant = verdict.Compliant
	missing := map[string]bool{}
	for _, r := range verdict.Results {
		// A policy that did not apply is not a policy that passed, and is not
		// a policy anyone needs to act on either — it is silent in both
		// directions, which is what `applies` means.
		if !r.Applies || len(r.Missing) == 0 {
			continue
		}
		p.Policies = append(p.Policies, r.Policy)
		for _, t := range r.Missing {
			missing[t] = true
		}
	}
	for t := range missing {
		p.Missing = append(p.Missing, t)
	}
	// Sorted, because these are read side by side across Bundles and a list
	// that reorders itself between loads is one people compare wrongly.
	sort.Strings(p.Missing)
	sort.Strings(p.Policies)
	return p
}

func unknownForAll(bundles []string, why string) []Preflight {
	out := make([]Preflight, 0, len(bundles))
	for _, b := range bundles {
		out = append(out, Preflight{Bundle: b, Unknown: why})
	}
	return out
}
