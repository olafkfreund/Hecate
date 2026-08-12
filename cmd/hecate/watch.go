package main

import (
	"context"
	"fmt"
	"time"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

// watchInterval is how often a crossing in flight is re-read.
//
// A second is chosen for a human watching a terminal, not for the API server:
// one GET per second against a single object is nothing, and anything slower
// makes the output feel like it has stopped.
const watchInterval = time.Second

// watchPassage follows a crossing to its end, printing each step as it settles,
// and returns the exit code for what happened.
//
// **The exit code is the point.** Without it `--watch` is decoration: a CI job
// that promotes and waits needs to know whether the thing it asked for landed,
// and `hecate promote` alone can only tell it that a Passage was opened.
//
// Polling rather than watching the API: a Passage is one object, the crossing
// takes minutes, and a watch would bring reconnection and bookmark handling for
// no gain a human would notice.
func watchPassage(ctx context.Context, o *ops.Ops, namespace, name string, timeout time.Duration) int {
	deadline := ctx.Done()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	// What we have already printed, so a step that stays Running for ten
	// minutes is announced once rather than six hundred times.
	printed := map[int]bool{}

	tick := time.NewTicker(watchInterval)
	defer tick.Stop()

	for {
		p, err := o.Passage(ctx, namespace, name)
		if err != nil {
			// A Passage that vanishes mid-crossing is a real answer, not a
			// transient: Gate retention collects finished ones (D40), so this
			// is most likely a crossing that finished long enough ago to be
			// tidied away.
			if ops.IsNotFound(err) {
				return fail(exitError, "%s is gone — it may have been collected", name)
			}
			return fail(exitError, "%s", err)
		}

		printSettledSteps(p, printed)

		if p.Status.Phase.Terminal() {
			return reportOutcome(p)
		}

		select {
		case <-tick.C:
		case <-timer:
			// Deliberately not an error about Hecate: the crossing is still
			// going, and saying so is more useful than implying it failed.
			fmt.Printf("\nstill crossing after %s — %s\n", timeout, name)
			fmt.Printf("  hecate explain %s\n", p.Spec.Gate)
			return exitError
		case <-deadline:
			fmt.Printf("\nstopped watching; %s is still crossing\n", name)
			return exitError
		}
	}
}

// printSettledSteps announces steps that have reached a terminal phase since
// the last look, in spec order.
func printSettledSteps(p *v1alpha1.Passage, printed map[int]bool) {
	for i, st := range p.Status.Steps {
		if printed[i] || !st.Phase.Terminal() {
			continue
		}
		printed[i] = true
		fmt.Printf("  %-16s %-10s %s\n", st.Uses, st.Phase, st.Message)
	}
}

// reportOutcome prints the final line and returns the exit code.
//
// A failed crossing is exitCrossingFailed rather than exitError, because the
// two call for different things: one means the promotion did not land, the
// other means Hecate could not tell you whether it did. A pipeline branches on
// that difference, which is the whole reason these codes are documented.
func reportOutcome(p *v1alpha1.Passage) int {
	switch p.Status.Phase {
	case v1alpha1.PassageSucceeded:
		fmt.Printf("\n%s crossed %s\n", p.Spec.Bundle, p.Spec.Gate)
		return exitOK
	case v1alpha1.PassageAborted:
		fmt.Printf("\naborted crossing %s through %s\n", p.Spec.Bundle, p.Spec.Gate)
		return exitCrossingFailed
	default:
		fmt.Printf("\n%s did not cross %s: %s\n", p.Spec.Bundle, p.Spec.Gate, p.Status.Message)
		fmt.Printf("  hecate explain %s\n", p.Spec.Gate)
		return exitCrossingFailed
	}
}
