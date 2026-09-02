package mcp

import (
	"encoding/json"
	"testing"

	"github.com/olafkfreund/hecate/pkg/ops"
)

// whyStuckOutputSchema decodes the published why_stuck outputSchema's kind
// and state enums, so tests can check them against pkg/ops's canonical lists
// instead of a hand-copied set of expected strings — a hand-copied expectation
// here would just be a fourth copy of the same set (see D64).
func whyStuckOutputSchema(t *testing.T) (states, kinds []string) {
	t.Helper()

	tools := ReadTools(nil, "default")
	var tool *Tool
	for i := range tools {
		if tools[i].Name == "why_stuck" {
			tool = &tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("no why_stuck tool registered")
	}

	var decoded struct {
		Properties struct {
			State struct {
				Enum []string `json:"enum"`
			} `json:"state"`
			Blockers struct {
				Items struct {
					Properties struct {
						Kind struct {
							Enum []string `json:"enum"`
						} `json:"kind"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"blockers"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.OutputSchema, &decoded); err != nil {
		t.Fatalf("decoding why_stuck outputSchema: %v", err)
	}
	return decoded.Properties.State.Enum, decoded.Properties.Blockers.Items.Properties.Kind.Enum
}

// TestWhyStuckSchemaCoversEveryBlockerKind is the regression test for the
// published outputSchema drifting from pkg/ops's BlockerKind constants — see
// D64. It fails against a hand-typed schema that is missing an entry (as
// tools.go was missing BlockerChangeHeld, added by D49/#30) because it walks
// ops.AllBlockerKinds rather than repeating the set a third time.
func TestWhyStuckSchemaCoversEveryBlockerKind(t *testing.T) {
	_, kinds := whyStuckOutputSchema(t)

	published := map[string]bool{}
	for _, k := range kinds {
		published[k] = true
	}
	for _, want := range ops.AllBlockerKinds {
		if !published[string(want)] {
			t.Errorf("why_stuck outputSchema's kind enum is missing %q", want)
		}
	}
	if got, want := len(kinds), len(ops.AllBlockerKinds); got != want {
		t.Errorf("why_stuck outputSchema's kind enum has %d entries, want %d (ops.AllBlockerKinds)", got, want)
	}
}

func TestWhyStuckSchemaCoversEveryState(t *testing.T) {
	states, _ := whyStuckOutputSchema(t)

	published := map[string]bool{}
	for _, s := range states {
		published[s] = true
	}
	for _, want := range ops.AllStates {
		if !published[string(want)] {
			t.Errorf("why_stuck outputSchema's state enum is missing %q", want)
		}
	}
	if got, want := len(states), len(ops.AllStates); got != want {
		t.Errorf("why_stuck outputSchema's state enum has %d entries, want %d (ops.AllStates)", got, want)
	}
}
