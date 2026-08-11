package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// Diagnose asks a model to explain, in prose, a Gate that Hecate has already
// analysed.
//
// It is given the finished Explanation rather than the raw resources, and that
// is the important part: the deterministic analysis has already decided what is
// blocking and what would fix it, so the model's job is to say it readably. It
// is not being asked what is wrong, because a model that concluded something
// different from the code would be reporting a second opinion nobody asked for.
//
// The Explanation's own fields are Hecate's, but the strings inside them are
// not: a step message carries whatever a git host or a Flux controller said, so
// the whole thing goes inside the fence.
func Diagnose(ctx context.Context, c *Client, ex *ops.Explanation) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm: no model configured")
	}

	facts, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return "", fmt.Errorf("llm: encoding the explanation: %w", err)
	}

	fence := NewFence(fenceToken(ex))
	prompt := Prompt{
		Task: `Below is a promotion gate's analysed state, produced by Hecate itself.
The blockers and fixes have already been determined; do not second-guess them.

Write at most four sentences for an engineer who has just been paged:
say what is happening, why it is stuck, and what to do next.
Prefer the fix already given over inventing one.
If the state shows nothing wrong, say so plainly rather than manufacturing a concern.
Do not repeat the JSON, and do not claim to have taken any action.`,
		Blocks: []string{fence.Wrap("GATE STATE", string(facts))},
	}
	return c.Complete(ctx, prompt)
}

// fenceToken derives a per-prompt delimiter suffix from the subject.
//
// Not a secret and not random: it only has to be something the untrusted
// content is unlikely to contain by accident, and reproducible so that a prompt
// can be inspected after the fact. Anything matching it is stripped from the
// content before fencing, so guessing it does not help either.
func fenceToken(ex *ops.Explanation) string {
	return fmt.Sprintf("%s-%s-%d", "HECATE", ex.Gate, len(ex.Blockers))
}
