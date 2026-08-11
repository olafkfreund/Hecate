// Package metrics publishes Hecate's delivery metrics.
//
// controller-runtime already exports reconcile counts, durations, errors and
// workqueue depth — everything needed to answer "is the controller working".
// These answer the different question: "is delivery working". The DORA four
// derive from them, which is why they are shaped around crossings rather than
// reconciles.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// crossingBuckets span a second to about two hours. A crossing that waits on a
// pull request review is not an outlier to be clipped — it is the case the
// histogram most needs to represent honestly.
var crossingBuckets = prometheus.ExponentialBuckets(1, 2.5, 12)

var (
	// PassageDuration measures a whole crossing, start to terminal phase.
	PassageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hecate_passage_duration_seconds",
		Help:    "Time from a Passage starting to reaching a terminal phase.",
		Buckets: crossingBuckets,
	}, []string{"namespace", "gate", "outcome"})

	// PassagesTotal counts crossings by outcome. Deployment frequency and
	// change-failure rate both come from this one series.
	PassagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hecate_passages_total",
		Help: "Passages that reached a terminal phase, by outcome.",
	}, []string{"namespace", "gate", "outcome"})

	// StepDuration measures individual steps.
	//
	// Deliberately generic rather than a bespoke "flux convergence" metric:
	// step="flux-wait" *is* how long Flux took to converge, and every other
	// step gets measured for free. A special case would have cost more code and
	// answered fewer questions.
	StepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hecate_step_duration_seconds",
		Help:    "Time a Passage step took, from first invocation to terminal phase.",
		Buckets: crossingBuckets,
	}, []string{"namespace", "gate", "step", "outcome"})

	// GateHealth is one series per state, exactly one of which is 1.
	//
	// The alternative — a single gauge mapping states to numbers — forces every
	// dashboard and alert to hard-code that mapping, and silently means the
	// wrong thing the day a state is added.
	GateHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hecate_gate_health",
		Help: "Gate health, 1 for the current state and 0 for the others.",
	}, []string{"namespace", "gate", "status"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(PassageDuration, PassagesTotal, StepDuration, GateHealth)
}

// healthStates is the full set, so every series is published rather than
// appearing only once a Gate first enters that state. A gauge that springs into
// existence mid-incident breaks alerting rules that reference it.
var healthStates = []v1alpha1.Health{
	v1alpha1.HealthHealthy,
	v1alpha1.HealthProgressing,
	v1alpha1.HealthDegraded,
	v1alpha1.HealthUnknown,
	v1alpha1.HealthNotApplicable,
}

// RecordGateHealth publishes a Gate's current health.
func RecordGateHealth(namespace, gate string, current v1alpha1.Health) {
	for _, state := range healthStates {
		value := 0.0
		if state == current {
			value = 1.0
		}
		GateHealth.WithLabelValues(namespace, gate, string(state)).Set(value)
	}
}

// ForgetGate removes a deleted Gate's series. Without this a Gate that is
// removed keeps reporting its last health for ever, and an alert on it never
// clears.
func ForgetGate(namespace, gate string) {
	for _, state := range healthStates {
		GateHealth.DeleteLabelValues(namespace, gate, string(state))
	}
}

// RecordPassage publishes a finished crossing.
func RecordPassage(namespace, gate string, phase v1alpha1.PassagePhase, seconds float64) {
	outcome := string(phase)
	PassagesTotal.WithLabelValues(namespace, gate, outcome).Inc()
	// A negative or absent duration means the timestamps were not both set;
	// recording it would corrupt the histogram with nonsense.
	if seconds >= 0 {
		PassageDuration.WithLabelValues(namespace, gate, outcome).Observe(seconds)
	}
}

// RecordStep publishes a finished step.
func RecordStep(namespace, gate, step string, phase v1alpha1.StepPhase, seconds float64) {
	if seconds < 0 {
		return
	}
	StepDuration.WithLabelValues(namespace, gate, step, string(phase)).Observe(seconds)
}
