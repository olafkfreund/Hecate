package passage

import (
	"errors"
	"fmt"
	"testing"
)

// A failure with no reason code is one nothing downstream can act on, so every
// path must yield something classifiable.
func TestReasonOf(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"reasoned failure":    {Fail("GitAuthFailed", "denied"), "GitAuthFailed"},
		"reasoned terminal":   {FailTerminal("InvalidConfig", "bad"), "InvalidConfig"},
		"unreasoned terminal": {Terminalf("something broke"), ReasonUnknown},
		"plain error":         {errors.New("boom"), ReasonUnknown},
		"wrapped reasoned":    {fmt.Errorf("context: %w", Fail("FluxDegraded", "x")), "FluxDegraded"},
		"no error":            {nil, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ReasonOf(tt.err); got != tt.want {
				t.Errorf("ReasonOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// Terminality and reason are independent: a retryable failure can still be
// classified, which is the whole point of separating them.
func TestTerminalIsIndependentOfReason(t *testing.T) {
	retryable := Fail("RegistryUnreachable", "connection refused")
	if IsTerminal(retryable) {
		t.Error("Fail must produce a retryable error")
	}
	if ReasonOf(retryable) != "RegistryUnreachable" {
		t.Error("a retryable failure must still carry its reason")
	}

	fatal := FailTerminal("InvalidConfig", "no resources")
	if !IsTerminal(fatal) {
		t.Error("FailTerminal must produce a terminal error")
	}

	// Wrapping must not lose terminality, or a step's error becomes retryable
	// the moment someone adds context to it.
	if !IsTerminal(fmt.Errorf("while validating: %w", fatal)) {
		t.Error("wrapping lost terminality")
	}
}

func TestStepErrorMessage(t *testing.T) {
	if got := Fail("R", "the %s failed", "thing").Error(); got != "the thing failed" {
		t.Errorf("Error() = %q", got)
	}
	// A reason with no wrapped error still says something.
	if got := (&StepError{Reason: "Bare"}).Error(); got != "Bare" {
		t.Errorf("Error() = %q, want the reason", got)
	}
}
