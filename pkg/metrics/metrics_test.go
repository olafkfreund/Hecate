package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// hasLabel reports whether any StepDuration series carries the given label.
// Walking the collected metrics rather than a formatted dump: the text
// encoding is a moving target and this asserts on the thing that matters.
func hasLabel(t *testing.T, name, value string) bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 32)
	StepDuration.Collect(ch)
	close(ch)
	for m := range ch {
		var got dto.Metric
		if err := m.Write(&got); err != nil {
			t.Fatal(err)
		}
		for _, l := range got.Label {
			if l.GetName() == name && l.GetValue() == value {
				return true
			}
		}
	}
	return false
}

func TestGateHealthPublishesEveryState(t *testing.T) {
	GateHealth.Reset()
	RecordGateHealth("acme", "production", v1alpha1.HealthDegraded)

	// Every state is published, not only the current one: a gauge that springs
	// into existence mid-incident breaks alerting rules that reference it.
	if got := testutil.CollectAndCount(GateHealth); got != len(healthStates) {
		t.Errorf("published %d series, want %d — one per state", got, len(healthStates))
	}
	if v := testutil.ToFloat64(GateHealth.WithLabelValues("acme", "production", "Degraded")); v != 1 {
		t.Errorf("Degraded = %v, want 1", v)
	}
	if v := testutil.ToFloat64(GateHealth.WithLabelValues("acme", "production", "Healthy")); v != 0 {
		t.Errorf("Healthy = %v, want 0 while Degraded", v)
	}
}

// Exactly one state is 1 at any moment, including after a transition.
func TestGateHealthTransitionClearsThePrevious(t *testing.T) {
	GateHealth.Reset()
	RecordGateHealth("acme", "production", v1alpha1.HealthHealthy)
	RecordGateHealth("acme", "production", v1alpha1.HealthDegraded)

	if v := testutil.ToFloat64(GateHealth.WithLabelValues("acme", "production", "Healthy")); v != 0 {
		t.Errorf("Healthy = %v after transitioning away, want 0", v)
	}
	if v := testutil.ToFloat64(GateHealth.WithLabelValues("acme", "production", "Degraded")); v != 1 {
		t.Errorf("Degraded = %v, want 1", v)
	}
}

// A deleted Gate that keeps reporting its last health means an alert on it
// never clears.
func TestForgetGateRemovesTheSeries(t *testing.T) {
	GateHealth.Reset()
	RecordGateHealth("acme", "gone", v1alpha1.HealthHealthy)
	if testutil.CollectAndCount(GateHealth) == 0 {
		t.Fatal("nothing was published")
	}

	ForgetGate("acme", "gone")
	if got := testutil.CollectAndCount(GateHealth); got != 0 {
		t.Errorf("%d series survived deletion", got)
	}
}

func TestRecordPassage(t *testing.T) {
	PassagesTotal.Reset()
	PassageDuration.Reset()

	RecordPassage("acme", "production", v1alpha1.PassageSucceeded, 42)
	RecordPassage("acme", "production", v1alpha1.PassageFailed, 7)
	RecordPassage("acme", "production", v1alpha1.PassageSucceeded, 12)

	if v := testutil.ToFloat64(PassagesTotal.WithLabelValues("acme", "production", "Succeeded")); v != 2 {
		t.Errorf("Succeeded count = %v, want 2", v)
	}
	if v := testutil.ToFloat64(PassagesTotal.WithLabelValues("acme", "production", "Failed")); v != 1 {
		t.Errorf("Failed count = %v, want 1", v)
	}
	// Both outcomes counted separately is what makes change-failure rate a
	// division rather than a guess.
	if got := testutil.CollectAndCount(PassageDuration); got != 2 {
		t.Errorf("%d duration series, want one per outcome", got)
	}
}

// Timestamps are not always both set; recording the difference anyway would put
// nonsense in the histogram.
func TestUnsetTimestampsAreNotRecorded(t *testing.T) {
	PassageDuration.Reset()
	PassagesTotal.Reset()
	RecordPassage("acme", "production", v1alpha1.PassageAborted, -1)

	if got := testutil.CollectAndCount(PassageDuration); got != 0 {
		t.Errorf("recorded %d duration observations for an unmeasurable Passage", got)
	}
	// The crossing still happened, so it is still counted.
	if v := testutil.ToFloat64(PassagesTotal.WithLabelValues("acme", "production", "Aborted")); v != 1 {
		t.Errorf("Aborted count = %v, want 1 — the crossing happened even if untimed", v)
	}

	StepDuration.Reset()
	RecordStep("acme", "production", "flux-wait", v1alpha1.StepSucceeded, -1)
	if got := testutil.CollectAndCount(StepDuration); got != 0 {
		t.Errorf("recorded %d step observations for an unmeasurable step", got)
	}
}

// Flux convergence time is not a bespoke metric — it is this one, filtered.
func TestStepDurationCoversFluxConvergence(t *testing.T) {
	StepDuration.Reset()
	RecordStep("acme", "production", "flux-wait", v1alpha1.StepSucceeded, 95)
	RecordStep("acme", "production", "git-push", v1alpha1.StepSucceeded, 2)

	if got := testutil.CollectAndCount(StepDuration); got != 2 {
		t.Fatalf("%d step series, want one per step", got)
	}
	if !hasLabel(t, "step", "flux-wait") {
		t.Error("flux-wait is not distinguishable in the exported metric")
	}
}
